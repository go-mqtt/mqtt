package mqtt_test

import (
	"context"
	"errors"
	"fmt"
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

func init() {
	PublishAtLeastOnce = mqtttest.NewPublishAckStub(mqtt.ErrClosed)
	Subscribe = mqtttest.NewSubscribeStub(mqtt.ErrClosed)
}

// It is good practice to install the client from main.
func ExampleNewClient_setup() {
	client := mqtt.NewClient(&mqtt.Config{WireTimeout: time.Second}, mqtt.NewDialer("tcp", "localhost:1883"))
	if err := client.VolatileSession("demo-client"); err != nil {
		fmt.Print("exit on ", err)
		os.Exit(2)
	}

	// launch read-routine
	go func() {
		var big *mqtt.BigMessage
		for {
			message, channel, err := client.ReadSlices()
			switch {
			case err == nil:
				// do something with inbound message
				fmt.Printf("📥 %q: %q", channel, message)

			case errors.Is(err, mqtt.ErrClosed):
				return // terminated

			case mqtt.IsDeny(err):
				fmt.Print("unusable configuration: ", err)
				os.Exit(2)

			case errors.As(err, &big):
				fmt.Printf("%d byte content skipped", big.Size)

			case mqtt.IsConnectionRefused(err):
				fmt.Print(err)
				// ErrDown for a while
				time.Sleep(5*time.Minute - time.Second)

			default:
				fmt.Print("MQTT unavailable: ", err)
				// ErrDown for short backoff
				time.Sleep(2 * time.Second)
			}
		}
	}()

	// Install each method in use as a package variable.
	// Such setup allows for unit tests with stubs.
	Publish = client.Publish

	// apply signals
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		for sig := range signals {
			switch sig {
			case syscall.SIGINT:
				fmt.Print("MQTT close on SIGINT…")
				err := client.Close()
				if err != nil {
					fmt.Print(err)
				}

			case syscall.SIGTERM:
				fmt.Print("MQTT disconnect on SIGTERM…")
				err := client.Disconnect(nil)
				if err != nil {
					fmt.Print(err)
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
			fmt.Print("alert submitted")

		case mqtt.IsDeny(err), errors.Is(err, mqtt.ErrClosed):
			fmt.Print("🚨 alert not send: ", err)
			return

		case errors.Is(err, mqtt.ErrMax), errors.Is(err, mqtt.ErrDown):
			fmt.Print("⚠️ alert delay: ", err)
			time.Sleep(time.Second / 4)
			continue

		default:
			fmt.Print("⚠️ alert delay on persistence malfunction: ", err)
			time.Sleep(time.Second)
			continue
		}

		for err := range ack {
			if errors.Is(err, mqtt.ErrClosed) {
				fmt.Print("🚨 alert suspended: ", err)
				// Submission will continue when the Client
				// is restarted with the same Store again.
				return
			}
			fmt.Print("⚠️ alert delay on connection malfunction: ", err)
		}
		fmt.Print("alert confirmed")
		break
	}
	// Output:
	// 🚨 alert not send: mqtt: client closed
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

		case errors.Is(err, mqtt.ErrMax), errors.Is(err, mqtt.ErrDown):
			time.Sleep(time.Second)

		default:
			backoff := 4 * time.Second
			fmt.Printf("subscribe retry in %s on: %s", backoff, err)
			time.Sleep(backoff)
		}
	}
	// Output:
	// no subscribe: mqtt: client closed
}
