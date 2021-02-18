package mqtt_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pascaldekloe/mqtt"
	"github.com/pascaldekloe/mqtt/mqtttest"
)

// Publish is a method from mqtt.Client.
var Publish func(quit <-chan struct{}, message []byte, topic string) error

// PublishAtLeastOnce is a method from mqtt.Client.
var PublishAtLeastOnce func(message []byte, topic string) (ack <-chan error, err error)

// Subscribe is a method from mqtt.Client.
var Subscribe func(quit <-chan struct{}, topicFilters ...string) error

// Online is a method from mqtt.Client.
var Online func() <-chan struct{}

func init() {
	PublishAtLeastOnce = mqtttest.NewPublishAckStub(nil)
	Subscribe = mqtttest.NewSubscribeStub(nil)
	Online = func() <-chan struct{} {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
}

// It is good practice to install the client from main.
func ExampleClient_setup() {
	client, err := mqtt.VolatileSession("demo-client", &mqtt.Config{
		Dialer:      mqtt.NewDialer("tcp", "localhost:1883"),
		WireTimeout: time.Second,
	})
	if err != nil {
		log.Fatal("exit on broken setup: ", err)
	}

	// launch read-routine
	go func() {
		var big *mqtt.BigMessage
		for {
			message, channel, err := client.ReadSlices()
			switch {
			case err == nil:
				// do something with inbound message
				log.Printf("📥 %q: %q", channel, message)

			case errors.Is(err, mqtt.ErrClosed):
				log.Print(err)
				return // terminated

			case errors.As(err, &big):
				log.Printf("%d byte content skipped", big.Size)

			case mqtt.IsConnectionRefused(err):
				log.Print(err)
				// ErrDown for a while
				time.Sleep(15*time.Minute - time.Second)

			default:
				log.Print("MQTT unavailable: ", err)
				// ErrDown for short backoff
				time.Sleep(2 * time.Second)
			}
		}
	}()

	// Install each method in use as a package variable. Such setup is
	// compatible with the tools proveded from the mqtttest subpackage.
	Publish = client.Publish

	// apply signals
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		for sig := range signals {
			switch sig {
			case syscall.SIGINT:
				log.Print("MQTT close on SIGINT…")
				err := client.Close()
				if err != nil {
					log.Print(err)
				}

			case syscall.SIGTERM:
				log.Print("MQTT disconnect on SIGTERM…")
				err := client.Disconnect(nil)
				if err != nil {
					log.Print(err)
				}
			}
		}
	}()

	// Output:
}

// Error scenario and how to act uppon them.
func ExampleClient_PublishAtLeastOnce_hasty() {
	for {
		ack, err := PublishAtLeastOnce([]byte("🍸🆘"), "demo/alert")
		switch {
		case err == nil:
			fmt.Println("alert submitted")

		case mqtt.IsDeny(err), errors.Is(err, mqtt.ErrClosed):
			fmt.Println("🚨 alert not send:", err)
			return

		case errors.Is(err, mqtt.ErrDown):
			fmt.Println("⚠️ alert delay:", err)
			<-Online()

		case errors.Is(err, mqtt.ErrMax):
			fmt.Println("⚠️ alert delay:", err)
			time.Sleep(time.Second / 4)
			continue

		default:
			fmt.Println("⚠️ alert delay on persistence malfunction:", err)
			time.Sleep(time.Second)
			continue
		}

		for err := range ack {
			if errors.Is(err, mqtt.ErrClosed) {
				fmt.Println("🚨 alert suspended:", err)
				// Submission will continue when the Client
				// is restarted with the same Store again.
				return
			}
			fmt.Println("⚠️ alert delay on connection malfunction:", err)
		}
		fmt.Println("alert confirmed")
		break
	}
	// Output:
	// alert submitted
	// alert confirmed
}

// Error scenario and how to act uppon them.
func ExampleClient_Subscribe_sticky() {
	const topicFilter = "demo/+"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for {
		err := Subscribe(ctx.Done(), topicFilter)
		switch {
		case err == nil:
			fmt.Printf("subscribed to %q", topicFilter)
			return

		case mqtt.IsDeny(err), errors.Is(err, mqtt.ErrClosed):
			fmt.Print("no subscribe: ", err)
			return

		case errors.Is(err, mqtt.ErrCanceled), errors.Is(err, mqtt.ErrAbandoned):
			fmt.Print("subscribe timeout: ", err)
			return

		case errors.Is(err, mqtt.ErrDown):
			<-Online()

		case errors.Is(err, mqtt.ErrMax):
			time.Sleep(time.Second / 2)

		default:
			backoff := 4 * time.Second
			fmt.Printf("subscribe retry in %s on: %s", backoff, err)
			time.Sleep(backoff)
		}
	}
	// Output:
	// subscribed to "demo/+"
}
