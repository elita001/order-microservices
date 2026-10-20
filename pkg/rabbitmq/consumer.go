package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/elita001/order-microservices/pkg/logger"
)

// Delivery is a received message. Handlers get only what they need, not the AMQP types.
type Delivery struct {
	MessageID   string
	RoutingKey  string
	Body        []byte
	Redelivered bool
}

// Handler processes a delivery. A nil error acknowledges it, a Permanent error
// rejects it without requeueing, and any other error requeues it for another attempt.
// Delivery is at-least-once, so handlers must be idempotent.
type Handler func(ctx context.Context, d Delivery) error

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks an error that retrying cannot fix, such as a malformed message.
func Permanent(err error) error {
	return permanentError{err: err}
}

func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Topology describes the durable queue a consumer reads and the events bound to it.
type Topology struct {
	Exchange    string
	Queue       string
	RoutingKeys []string
}

// ConsumerConfig is usually loaded from environment variables with the CONSUMER_ prefix.
type ConsumerConfig struct {
	Prefetch       int           `env:"PREFETCH" envDefault:"10"`         // unacknowledged messages per connection
	Concurrency    int           `env:"CONCURRENCY" envDefault:"4"`       // messages handled in parallel
	HandlerTimeout time.Duration `env:"HANDLER_TIMEOUT" envDefault:"10s"` // a started handler finishes even during shutdown
	RequeueDelay   time.Duration `env:"REQUEUE_DELAY" envDefault:"1s"`    // pause before requeueing a failed message
	MaxBackoff     time.Duration `env:"MAX_BACKOFF" envDefault:"30s"`     // upper bound for the pause between reconnects
}

// Consumer reads messages from a queue and reconnects automatically. On each connection
// it declares the exchange, the queue, and the bindings, so services can start in any order.
type Consumer struct {
	url      string
	name     string
	topology Topology
	cfg      ConsumerConfig
	log      *slog.Logger
}

func NewConsumer(url, name string, topology Topology, cfg ConsumerConfig, log *slog.Logger) *Consumer {
	return &Consumer{
		url:      url,
		name:     name,
		topology: topology,
		cfg:      cfg,
		log:      log.With("component", "rabbitmq_consumer", "queue", topology.Queue),
	}
}

// Run consumes messages until ctx is canceled. Connection failures are logged and
// retried with exponential backoff.
func (c *Consumer) Run(ctx context.Context, handler Handler) error {
	const initialBackoff = time.Second
	delay := initialBackoff
	for {
		connected, err := c.consume(ctx, handler)
		if ctx.Err() != nil {
			c.log.Info("consumer stopped")
			return nil
		}
		if connected {
			delay = initialBackoff
		}
		c.log.Error("consumer disconnected, reconnecting", "error", err, "retry_in", delay.String())

		select {
		case <-ctx.Done():
			c.log.Info("consumer stopped")
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, c.cfg.MaxBackoff)
	}
}

// consume runs one connection session. It returns when ctx is canceled (nil error)
// or when the connection or channel is lost.
func (c *Consumer) consume(ctx context.Context, handler Handler) (connected bool, err error) {
	conn, err := Dial(c.url, c.name)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return false, fmt.Errorf("open channel: %w", err)
	}
	if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return false, fmt.Errorf("set prefetch: %w", err)
	}
	if err := c.declare(ch); err != nil {
		return false, err
	}

	tag := c.name + "-" + uuid.NewString()
	deliveries, err := ch.Consume(c.topology.Queue, tag, false, false, false, false, nil)
	if err != nil {
		return false, fmt.Errorf("consume %q: %w", c.topology.Queue, err)
	}
	c.log.Info("consumer connected", "routing_keys", c.topology.RoutingKeys, "concurrency", c.cfg.Concurrency)

	// The deliveries channel is closed when the consumer is canceled or the connection
	// is lost; workers exit after handling everything that was already received.
	var wg sync.WaitGroup
	for range c.cfg.Concurrency {
		wg.Go(func() {
			for d := range deliveries {
				c.handle(ctx, handler, d)
			}
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		// Stop new deliveries and wait for in-flight handlers.
		if err := ch.Cancel(tag, false); err != nil {
			c.log.Warn("cancel consumer", "error", err)
		}
		<-done
		return true, nil
	case <-done:
		return true, errors.New("delivery channel closed: connection or channel lost")
	}
}

func (c *Consumer) declare(ch *amqp.Channel) error {
	if err := DeclareExchange(ch, c.topology.Exchange); err != nil {
		return err
	}
	// Quorum queues are replicated, durable, and the recommended queue type in RabbitMQ 4.
	// They also limit redeliveries (20 by default), so a poison message cannot loop forever.
	if _, err := ch.QueueDeclare(c.topology.Queue, true, false, false, false, amqp.Table{
		amqp.QueueTypeArg: amqp.QueueTypeQuorum,
	}); err != nil {
		return fmt.Errorf("declare queue %q: %w", c.topology.Queue, err)
	}
	for _, key := range c.topology.RoutingKeys {
		if err := ch.QueueBind(c.topology.Queue, key, c.topology.Exchange, false, nil); err != nil {
			return fmt.Errorf("bind queue %q to %q: %w", c.topology.Queue, key, err)
		}
	}
	return nil
}

func (c *Consumer) handle(ctx context.Context, handler Handler, d amqp.Delivery) {
	log := c.log.With("message_id", d.MessageId, "routing_key", d.RoutingKey, "redelivered", d.Redelivered)

	if ctx.Err() != nil {
		// Shutting down: return prefetched messages to the queue for other consumers.
		nack(log, d, true)
		return
	}

	// A started handler is not interrupted by shutdown, only by its own timeout.
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.HandlerTimeout)
	defer cancel()

	err := handler(logger.WithContext(hctx, log), Delivery{
		MessageID:   d.MessageId,
		RoutingKey:  d.RoutingKey,
		Body:        d.Body,
		Redelivered: d.Redelivered,
	})
	switch {
	case err == nil:
		if err := d.Ack(false); err != nil {
			log.Error("ack message", "error", err)
		}
	case IsPermanent(err):
		log.Error("message rejected", "error", err)
		nack(log, d, false)
	default:
		// The pause prevents a hot loop while a dependency (e.g., the database) is down.
		log.Warn("message processing failed, requeueing", "error", err, "requeue_in", c.cfg.RequeueDelay.String())
		select {
		case <-ctx.Done():
		case <-time.After(c.cfg.RequeueDelay):
		}
		nack(log, d, true)
	}
}

func nack(log *slog.Logger, d amqp.Delivery, requeue bool) {
	if err := d.Nack(false, requeue); err != nil {
		log.Error("nack message", "error", err, "requeue", requeue)
	}
}
