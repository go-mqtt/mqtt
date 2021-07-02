// Package main provides a command-line utility.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-mqtt/mqtt"
)

const messageMax = 256 * 1024 * 1024

// ANSI escape codes for markup.
const (
	bold   = "\x1b[1m"
	italic = "\x1b[3m"
	clear  = "\x1b[0m"
)

// Name of the invoked executable.
var name = os.Args[0]

var subscribeFlags []string

func init() {
	flag.Func("subscribe", "Listen with a topic `filter`. Inbound messages are printed to\n"+italic+"standard output"+clear+" until interrupted by a signal(3). Multiple\n"+bold+"-subscribe"+clear+" options may be applied together.", func(value string) error {
		subscribeFlags = append(subscribeFlags, value)
		return nil
	})
}

const generatedLabel = "generated"

var (
	publishFlag = flag.String("publish", "", "Send a message to a `topic`. The payload is read from "+italic+"standard\ninput"+clear+".")

	timeoutFlag = flag.Duration("timeout", 4*time.Second, "Network operation expiry.")
	netFlag     = flag.String("net", "tcp", "Select the network by `name`. Valid alternatives include tcp4,\ntcp6 and unix.")

	tlsFlag    = flag.Bool("tls", false, "Secure the connection with TLS.")
	serverFlag = flag.String("server", "", "Use a specific server `name` with TLS")
	caFlag     = flag.String("ca", "", "Amend the trusted certificate authorities with a PEM `file`.")
	certFlag   = flag.String("cert", "", "Use a client certificate from a PEM `file` (with a corresponding\n"+bold+"-key"+clear+" option).")
	keyFlag    = flag.String("key", "", "Use a private key (matching the client certificate) from a PEM\n`file`.")

	userFlag = flag.String("user", "", "The user `name` may be used by the broker for authentication\nand/or authorization purposes.")
	passFlag = flag.String("pass", "", "The `file` content is used as a password.")

	clientFlag = flag.String("client", generatedLabel, "Use a specific client `identifier`.")

	prefixFlag = flag.String("prefix", "", "Print a `string` before each inbound message.")
	suffixFlag = flag.String("suffix", "\n", "Print a `string` after each inbound message.")
	topicFlag  = flag.Bool("topic", false, "Print the respective topic of each inbound message.")
	quoteFlag  = flag.Bool("quote", false, "Print inbound topics and messages as quoted strings.")

	quietFlag   = flag.Bool("quiet", false, "Suppress all output to "+italic+"standard error"+clear+". Error reporting is\ndeduced to the exit code only.")
	verboseFlag = flag.Bool("verbose", false, "Produces more output to "+italic+"standard error"+clear+" for debug purposes.")
)

// Config collects the command arguments.
func Config() (clientID string, config *mqtt.Config) {
	var addr string
	switch args := flag.Args(); {
	case len(args) == 0:
		printManual()
		os.Exit(2)

	case len(args) == 1:
		addr = args[0]

	default:
		log.Printf("%s: multiple address arguments %q", name, args)
		os.Exit(2)
	}

	var TLS *tls.Config
	if *tlsFlag {
		TLS = new(tls.Config)
	}

	if *serverFlag != "" {
		if TLS == nil {
			log.Fatal(name, ": -server requires -tls option")
		}
		TLS.ServerName = *serverFlag
	}

	switch {
	case *certFlag != "" && *keyFlag != "":
		if TLS == nil {
			log.Fatal(name, ": -cert requires -tls option")
		}

		certPEM, err := os.ReadFile(*certFlag)
		if err != nil {
			log.Fatal(err)
		}
		keyPEM, err := os.ReadFile(*keyFlag)
		if err != nil {
			log.Fatal(err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			log.Fatal(name, ": unusable -cert and -key content; ", err)
		}
		TLS.Certificates = append(TLS.Certificates, cert)

	case *certFlag != "":
		log.Fatal(name, ": -cert requires -key option")
	case *keyFlag != "":
		log.Fatal(name, ": -key requires -cert option")
	}

	if *caFlag != "" {
		if TLS == nil {
			log.Fatal(name, ": -ca requires -tls option")
		}

		if certs, err := x509.SystemCertPool(); err != nil {
			log.Print(name, ": system certificates unavailable; ", err)
			TLS.RootCAs = x509.NewCertPool()
		} else {
			TLS.RootCAs = certs
		}

		text, err := os.ReadFile(*caFlag)
		if err != nil {
			log.Fatal(err)
		}
		for n := 1; ; n++ {
			var block *pem.Block
			block, text = pem.Decode(text)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				log.Printf("%s: ignoring PEM block № %d of type %q in %s", name, n, block.Type, *caFlag)
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				log.Printf("%s: ignoring PEM block № %d in %s; %s", name, n, *caFlag, err)
				continue
			}
			TLS.RootCAs.AddCert(cert)
		}
	}

	if _, _, err := net.SplitHostPort(addr); err != nil {
		port := "1883"
		if TLS != nil {
			port = "8883"
		}
		addr = net.JoinHostPort(addr, port)
	}

	clientID = *clientFlag
	if clientID == generatedLabel {
		clientID = "mqttc(1)-" + time.Now().In(time.UTC).Format(time.RFC3339Nano)
	}

	config = &mqtt.Config{
		PauseTimeout: *timeoutFlag,
		UserName:     *userFlag,
	}
	if *passFlag != "" {
		bytes, err := os.ReadFile(*passFlag)
		if err != nil {
			log.Fatal(err)
		}
		config.Password = bytes
	}

	if TLS != nil {
		config.Dialer = mqtt.NewTLSDialer(*netFlag, addr, TLS)
	} else {
		config.Dialer = mqtt.NewDialer(*netFlag, addr)
	}
	return
}

var exitStatus = make(chan int, 1)

func failMQTT(client *mqtt.Client, err error) {
	log.Print(err)

	select {
	case exitStatus <- 1:
	default: // exit status already defined
	}

	err = client.Close()
	if err != nil {
		log.Print(err)
	}
}

func main() {
	log.SetFlags(0)
	flag.Usage = printManual
	flag.Parse()
	if *quietFlag {
		log.SetOutput(io.Discard)
	}

	clientID, config := Config()
	client, err := mqtt.VolatileSession(clientID, config)
	if err != nil {
		log.Fatal(err)
	}

	go applySignals(client)

	go execPubSub(client)

	// Read routine runs until mqtt.Client Close or Disconnect.
	var big *mqtt.BigMessage
	for {
		message, topic, err := client.ReadSlices()
		switch {
		case err == nil:
			printMessage(message, topic)

		case errors.Is(err, mqtt.ErrClosed):
			os.Exit(<-exitStatus)

		case errors.As(err, &big):
			message, err := big.ReadAll()
			if err != nil {
				failMQTT(client, err)
			} else {
				printMessage(message, big.Topic)
			}

		default:
			failMQTT(client, err)

			switch {
			case errors.Is(err, mqtt.ErrProtocolLevel):
				os.Exit(5)
			case errors.Is(err, mqtt.ErrClientID):
				os.Exit(6)
			case errors.Is(err, mqtt.ErrUnavailable):
				os.Exit(7)
			case errors.Is(err, mqtt.ErrAuthBad):
				os.Exit(8)
			case errors.Is(err, mqtt.ErrAuth):
				os.Exit(9)
			}
		}
	}
}

func printMessage(message, topic interface{}) {
	switch {
	case *topicFlag && *quoteFlag:
		fmt.Printf("%q%s%q%s", topic, *prefixFlag, message, *suffixFlag)
	case *topicFlag:
		fmt.Printf("%s%s%s%s", topic, *prefixFlag, message, *suffixFlag)
	case *quoteFlag:
		fmt.Printf("%s%q%s", *prefixFlag, message, *suffixFlag)
	default:
		fmt.Printf("%s%s%s", *prefixFlag, message, *suffixFlag)
	}
}

func execPubSub(client *mqtt.Client) {
	if *publishFlag != "" {
		// publish standard input
		message, err := io.ReadAll(io.LimitReader(os.Stdin, messageMax))
		switch {
		case err != nil:
			log.Fatal(name, ": ", err)
		case len(message) >= messageMax:
			log.Fatalf("%s: standard input reached %d byte limit", name, messageMax)
		}

		ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
		defer cancel()
		err = client.Publish(ctx.Done(), message, *publishFlag)
		switch {
		case err == nil:
			if *verboseFlag {
				log.Printf("%s: published %d bytes to %q", name, len(message), *publishFlag)
			}
		case errors.Is(err, mqtt.ErrClosed), errors.Is(err, mqtt.ErrDown):
			return
		default:
			failMQTT(client, err)
			return
		}
	}

	if len(subscribeFlags) != 0 {
		// subscribe & return
		ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
		defer cancel()
		err := client.SubscribeLimitAtMostOnce(ctx.Done(), subscribeFlags...)
		switch {
		case err == nil:
			if *verboseFlag {
				log.Printf("%s: subscribed to %d topic filters", name, len(subscribeFlags))
			}
		case errors.Is(err, mqtt.ErrClosed), errors.Is(err, mqtt.ErrDown):
			break
		default:
			failMQTT(client, err)
		}

		return
	}

	if *publishFlag == "" {
		// ping exchange
		ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
		defer cancel()
		err := client.Ping(ctx.Done())
		switch {
		case err == nil:
			break // OK
		case errors.Is(err, mqtt.ErrClosed), errors.Is(err, mqtt.ErrDown):
			return
		default:
			failMQTT(client, err)
			return
		}
	}

	// graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()
	err := client.Disconnect(ctx.Done())
	switch {
	case err == nil:
		exitStatus <- 0
	case errors.Is(err, mqtt.ErrClosed), errors.Is(err, mqtt.ErrDown):
		// exit status defined by cause
		break
	default:
		log.Print(err)
		exitStatus <- 1
	}
}

func applySignals(client *mqtt.Client) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	for sig := range signals {
		switch sig {
		case syscall.SIGINT:
			log.Print(name, ": SIGINT received")
			select {
			case exitStatus <- 130:
			default: // exit status already defined
			}
			err := client.Close()
			if err != nil {
				log.Print(err)
			}

		case syscall.SIGTERM:
			log.Print(name, ": SIGTERM received")
			ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
			defer cancel()
			err := client.Disconnect(ctx.Done())
			switch {
			case err == nil:
				exitStatus <- 143
			case errors.Is(err, mqtt.ErrClosed), errors.Is(err, mqtt.ErrDown):
				// exit status defined by cause
				break
			default:
				log.Print(err)
				exitStatus <- 1
			}
		}
	}
}

func printManual() {
	if *quietFlag {
		return
	}

	log.Print(bold + "NAME\n\t" + name + clear + " \u2014 MQTT broker access\n" +
		"\n" +
		bold + "SYNOPSIS\n" +
		"\t" + bold + name + clear + " [options] address\n" +
		"\n" +
		bold + "DESCRIPTION" + clear + "\n" +
		"\tThe command connects to the address argument, with an option to\n" +
		"\tpublish a message and/or subscribe with topic filters.\n" +
		"\n" +
		"\tWhen the address does not specify a port, then the defaults are\n" +
		"\tapplied, which is 1883 for plain connections and 8883 for TLS.\n" +
		"\n" +
		bold + "OPTIONS" + clear + "\n",
	)

	flag.PrintDefaults()

	log.Print("\n" + bold + "EXIT STATUS" + clear + "\n" +
		"\t(0) no error\n" +
		"\t(1) MQTT operational error\n" +
		"\t(2) illegal command invocation\n" +
		"\t(5) connection refused: unacceptable protocol version\n" +
		"\t(6) connection refused: identifier rejected\n" +
		"\t(7) connection refused: server unavailable\n" +
		"\t(8) connection refused: bad username or password\n" +
		"\t(9) connection refused: not authorized\n" +
		"\t(130) close on SIGINT\n" +
		"\t(143) disconnect on SIGTERM\n" +
		"\n" +

		bold + "EXAMPLES" + clear + "\n" +
		"\tSend a message:\n" +
		"\n" +
		"\t\techo \"hello\" | " + name + " -publish chat/misc localhost\n" +
		"\n" +
		"\tPrint messages:\n" +
		"\n" +
		"\t\t" + name + " -subscribe \"news/#\" -prefix \"📥 \" :1883\n" +
		"\n" +
		"\tHealth check:\n" +
		"\n" +
		"\t\t" + name + " -tls q1.example.com:8883 || echo \"exit $?\"\n" +
		"\n" +

		bold + "BUGS" + clear + "\n" +
		"\tReport bugs at <https://github.com/pascaldekloe/mqtt/issues>.\n" +
		"\n" +

		"SEE ALSO" + clear + "\n\tmosquitto_pub(1)\n",
	)
}
