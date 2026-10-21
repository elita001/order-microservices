// Command dlq inspects and replays a service's dead letter queue.
//
//	dlq list   -queue payment-service [-limit 20]
//	dlq replay -queue payment-service [-limit 100]
//
// replay moves messages back to the service queue with a fresh retry budget. Do it after
// fixing the cause; handlers are idempotent, so replaying is safe.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/elita001/order-microservices/pkg/rabbitmq"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "replay") {
		return errors.New("usage: dlq list|replay -queue <service queue> [-limit N] [-url amqp://...]")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	queue := flags.String("queue", "", "service queue, e.g. payment-service")
	limit := flags.Int("limit", 20, "maximum number of messages")
	url := flags.String("url", os.Getenv("RABBITMQ_URL"), "AMQP URL (default: $RABBITMQ_URL)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *queue == "" || *url == "" {
		return errors.New("-queue and -url (or RABBITMQ_URL) are required")
	}

	conn, err := rabbitmq.Dial(*url, "dlq-tool")
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}

	dlq := rabbitmq.DeadLetterQueue(*queue)
	info, err := ch.QueueDeclarePassive(dlq, true, false, false, false, amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum})
	if err != nil {
		return fmt.Errorf("queue %q not found: %w", dlq, err)
	}
	// Only the messages present now: replayed messages that fail again come back to the
	// DLQ, and the tool must not pick them up a second time.
	count := min(*limit, info.Messages)
	fmt.Printf("%s: %d message(s)\n\n", dlq, info.Messages)

	if command == "list" {
		return list(ch, dlq, count)
	}
	return replay(ch, *queue, dlq, count)
}

// list shows messages without removing them: they are not acknowledged and return to
// the queue when the channel closes.
func list(ch *amqp.Channel, dlq string, count int) error {
	for range count {
		d, ok, err := ch.Get(dlq, false)
		if err != nil {
			return fmt.Errorf("get message: %w", err)
		}
		if !ok {
			break
		}
		fmt.Printf("message_id:  %s\nevent:       %s\nretries:     %d\nfailed_at:   %v\nerror:       %v\nbody:        %s\n\n",
			d.MessageId, routingKey(d), rabbitmq.RetryCount(d.Headers), d.Headers[rabbitmq.HeaderFailedAt],
			reason(d), shorten(string(d.Body), 300))
	}
	return nil
}

func replay(ch *amqp.Channel, queue, dlq string, count int) error {
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("enable publisher confirms: %w", err)
	}
	replayed := 0
	for range count {
		d, ok, err := ch.Get(dlq, false)
		if err != nil {
			return fmt.Errorf("get message: %w", err)
		}
		if !ok {
			break
		}

		headers := amqp.Table{}
		for key, value := range d.Headers {
			headers[key] = value
		}
		for _, key := range []string{rabbitmq.HeaderRetryCount, rabbitmq.HeaderError, rabbitmq.HeaderLastError,
			rabbitmq.HeaderFailedAt, rabbitmq.HeaderOriginalQueue, "x-death", "x-first-death-exchange",
			"x-first-death-queue", "x-first-death-reason", "x-last-death-exchange", "x-last-death-queue", "x-last-death-reason"} {
			delete(headers, key)
		}
		headers[rabbitmq.HeaderOriginalRoutingKey] = routingKey(d)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		confirm, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", queue, false, false, amqp.Publishing{
			Headers: headers, ContentType: d.ContentType, DeliveryMode: amqp.Persistent,
			MessageId: d.MessageId, Type: d.Type, Timestamp: d.Timestamp, AppId: d.AppId, Body: d.Body,
		})
		if err == nil {
			var acked bool
			acked, err = confirm.WaitContext(ctx)
			if err == nil && !acked {
				err = errors.New("not acknowledged by the broker")
			}
		}
		cancel()
		if err != nil {
			_ = d.Nack(false, true)
			return fmt.Errorf("replay message %s: %w", d.MessageId, err)
		}
		if err := d.Ack(false); err != nil {
			return fmt.Errorf("ack message %s: %w", d.MessageId, err)
		}
		replayed++
		fmt.Printf("replayed %s (%s)\n", d.MessageId, routingKey(d))
	}
	fmt.Printf("\n%d message(s) moved to %s\n", replayed, queue)
	return nil
}

// routingKey is the event type: our header, or the routing key recorded by RabbitMQ when
// it dead-lettered the message itself (x-death, oldest entry last).
func routingKey(d amqp.Delivery) string {
	if key, ok := d.Headers[rabbitmq.HeaderOriginalRoutingKey].(string); ok && key != "" {
		return key
	}
	if deaths, ok := d.Headers["x-death"].([]any); ok && len(deaths) > 0 {
		if death, ok := deaths[len(deaths)-1].(amqp.Table); ok {
			if keys, ok := death["routing-keys"].([]any); ok && len(keys) > 0 {
				if key, ok := keys[0].(string); ok {
					return key
				}
			}
		}
	}
	return d.RoutingKey
}

func reason(d amqp.Delivery) any {
	if err, ok := d.Headers[rabbitmq.HeaderError]; ok {
		return err
	}
	if r, ok := d.Headers["x-first-death-reason"]; ok {
		return fmt.Sprintf("dead-lettered by RabbitMQ: %v", r)
	}
	return "unknown"
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
