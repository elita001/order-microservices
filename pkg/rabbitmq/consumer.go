package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/elita001/order-microservices/pkg/logger"
)

// Delivery is a received message. Handlers get only what they need, not the AMQP types.
type Delivery struct {
	MessageID string
	// RoutingKey is the event type. It survives retries: a message coming back from
	// a retry queue has the queue name as its AMQP routing key.
	RoutingKey  string
	Body        []byte
	Redelivered bool
	Retry       int // how many times processing has already failed
}

// Handler processes a delivery. A nil error acknowledges it, a Permanent error moves it
// to the dead letter queue at once, and any other error schedules a retry with backoff.
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
	// RetryDelays are the pauses before each retry; after the last one the message goes
	// to the dead letter queue. Every delay has its own retry queue.
	RetryDelays    []time.Duration `env:"RETRY_DELAYS" envDefault:"1s,5s,30s,2m,10m" envSeparator:","`
	RequeueDelay   time.Duration   `env:"REQUEUE_DELAY" envDefault:"1s"`   // pause before requeueing if forwarding to a retry queue fails
	MaxBackoff     time.Duration   `env:"MAX_BACKOFF" envDefault:"30s"`    // upper bound for the pause between reconnects
	HealthInterval time.Duration   `env:"HEALTH_INTERVAL" envDefault:"5s"` // how often the health check runs
}

// errPaused ends a session because the health check failed.
var errPaused = errors.New("consumption paused: dependency unavailable")

const (
	forwardTimeout     = 5 * time.Second
	healthCheckTimeout = 2 * time.Second
)

// Consumer reads messages from a queue and reconnects automatically. On each connection
// it declares the exchange, the queue with its retry and dead letter queues, and the
// bindings, so services can start in any order.
type Consumer struct {
	url         string
	name        string
	topology    Topology
	cfg         ConsumerConfig
	healthCheck func(ctx context.Context) error
	log         *slog.Logger
}

func NewConsumer(url, name string, topology Topology, cfg ConsumerConfig, log *slog.Logger) *Consumer {
	consumerPaused.WithLabelValues(topology.Queue).Set(0) // export 0, not "no data"
	return &Consumer{
		url:      url,
		name:     name,
		topology: topology,
		cfg:      cfg,
		log:      log.With("component", "rabbitmq_consumer", "queue", topology.Queue),
	}
}

// WithHealthCheck pauses consumption while check fails (e.g., the database is down):
// messages wait in the queue instead of failing and using up their retries.
func (c *Consumer) WithHealthCheck(check func(ctx context.Context) error) *Consumer {
	c.healthCheck = check
	return c
}

// Run consumes messages until ctx is canceled. Connection failures are logged and
// retried with exponential backoff.
func (c *Consumer) Run(ctx context.Context, handler Handler) error {
	const initialBackoff = time.Second
	delay := initialBackoff
	for {
		if !c.waitHealthy(ctx) {
			c.log.Info("consumer stopped")
			return nil
		}

		connected, err := c.consume(ctx, handler)
		if ctx.Err() != nil {
			c.log.Info("consumer stopped")
			return nil
		}
		if errors.Is(err, errPaused) {
			delay = initialBackoff
			continue // waitHealthy blocks until the dependency recovers
		}
		if connected {
			delay = initialBackoff
		}

		var amqpErr *amqp.Error
		if errors.As(err, &amqpErr) && amqpErr.Code == amqp.PreconditionFailed {
			c.log.Error("a queue exists with different arguments (declared by an older version); "+
				"delete the service's queues, e.g. with `make rabbitmq-reset-queues`", "error", err, "retry_in", delay.String())
		} else {
			c.log.Error("consumer disconnected, reconnecting", "error", err, "retry_in", delay.String())
		}

		select {
		case <-ctx.Done():
			c.log.Info("consumer stopped")
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, c.cfg.MaxBackoff)
	}
}

// waitHealthy returns true once the health check passes, or false if ctx is canceled.
func (c *Consumer) waitHealthy(ctx context.Context) bool {
	if c.healthCheck == nil || c.check(ctx) == nil {
		return true
	}
	paused := consumerPaused.WithLabelValues(c.topology.Queue)
	paused.Set(1)
	defer paused.Set(0)
	c.log.Warn("dependency unavailable, consumption is paused")
	ticker := time.NewTicker(c.cfg.HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		if c.check(ctx) == nil {
			c.log.Info("dependency recovered, resuming consumption")
			return true
		}
	}
}

func (c *Consumer) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	return c.healthCheck(ctx)
}

// consume runs one connection session. It returns when ctx is canceled (nil error),
// when the health check fails (errPaused), or when the connection or channel is lost.
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
	fwd, err := newForwarder(conn)
	if err != nil {
		return false, err
	}

	tag := c.name + "-" + uuid.NewString()
	deliveries, err := ch.Consume(c.topology.Queue, tag, false, false, false, false, nil)
	if err != nil {
		return false, fmt.Errorf("consume %q: %w", c.topology.Queue, err)
	}
	c.log.Info("consumer connected", "routing_keys", c.topology.RoutingKeys, "concurrency", c.cfg.Concurrency,
		"retry_delays", fmt.Sprint(c.cfg.RetryDelays))

	// session ends on shutdown or pause: prefetched messages that have not started yet
	// are returned to the queue.
	session, endSession := context.WithCancel(ctx)
	defer endSession()

	// The deliveries channel is closed when the consumer is canceled or the connection
	// is lost; workers exit after handling everything that was already received.
	var wg sync.WaitGroup
	for range c.cfg.Concurrency {
		wg.Go(func() {
			for d := range deliveries {
				c.handle(session, fwd, handler, d)
			}
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	paused := make(chan struct{})
	if c.healthCheck != nil {
		go c.watchHealth(session, paused)
	}

	stop := func() {
		endSession()
		if err := ch.Cancel(tag, false); err != nil {
			c.log.Warn("cancel consumer", "error", err)
		}
		<-done // in-flight handlers finish
	}
	select {
	case <-ctx.Done():
		stop()
		return true, nil
	case <-paused:
		stop()
		return true, errPaused
	case <-done:
		return true, errors.New("delivery channel closed: connection or channel lost")
	}
}

// watchHealth closes paused when the health check fails.
func (c *Consumer) watchHealth(ctx context.Context, paused chan<- struct{}) {
	ticker := time.NewTicker(c.cfg.HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := c.check(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("health check failed, pausing consumption", "error", err)
			close(paused)
			return
		}
	}
}

// declare creates the queue, its retry queues, and its dead letter queue (all quorum:
// replicated and durable, the recommended type in RabbitMQ 4):
//
//	queue ──failure──▶ queue.retry.<delay> ──TTL──▶ queue
//	queue ──permanent error or retries exhausted──▶ queue.dlq
//
// Retries are forwarded by the consumer (see handle). Dead-lettering configured on the
// queue itself is a safety net: a message that keeps returning to the queue (e.g., the
// process crashes while handling it) goes to the DLQ after deliveryLimit attempts.
func (c *Consumer) declare(ch *amqp.Channel) error {
	if err := DeclareExchange(ch, c.topology.Exchange); err != nil {
		return err
	}
	queue := c.topology.Queue
	if err := declareQuorumQueue(ch, DeadLetterQueue(queue), nil); err != nil {
		return err
	}
	if err := declareQuorumQueue(ch, queue, deadLetterTo(DeadLetterQueue(queue), amqp.Table{
		"x-delivery-limit": deliveryLimit,
	})); err != nil {
		return err
	}
	for _, delay := range c.cfg.RetryDelays {
		if err := declareQuorumQueue(ch, RetryQueue(queue, delay), deadLetterTo(queue, amqp.Table{
			"x-message-ttl": delay.Milliseconds(),
		})); err != nil {
			return err
		}
	}
	for _, key := range c.topology.RoutingKeys {
		if err := ch.QueueBind(queue, key, c.topology.Exchange, false, nil); err != nil {
			return fmt.Errorf("bind queue %q to %q: %w", queue, key, err)
		}
	}
	return nil
}

const deliveryLimit = 20

func declareQuorumQueue(ch *amqp.Channel, name string, args amqp.Table) error {
	if args == nil {
		args = amqp.Table{}
	}
	args[amqp.QueueTypeArg] = amqp.QueueTypeQuorum
	if _, err := ch.QueueDeclare(name, true, false, false, false, args); err != nil {
		return fmt.Errorf("declare queue %q: %w", name, err)
	}
	return nil
}

// deadLetterTo routes dead-lettered messages to the target queue via the default exchange.
// at-least-once dead-lettering (quorum queues only) does not lose a message if the
// target is temporarily unavailable.
func deadLetterTo(target string, args amqp.Table) amqp.Table {
	args["x-dead-letter-exchange"] = ""
	args["x-dead-letter-routing-key"] = target
	args["x-dead-letter-strategy"] = "at-least-once"
	args["x-overflow"] = "reject-publish" // required by at-least-once
	return args
}

func (c *Consumer) handle(session context.Context, fwd *forwarder, handler Handler, d amqp.Delivery) {
	retry := RetryCount(d.Headers)
	routingKey := OriginalRoutingKey(d)
	log := c.log.With("message_id", d.MessageId, "routing_key", routingKey, "retry", retry)

	if session.Err() != nil {
		// Shutting down or pausing: return the message to the queue for later.
		nack(log, d, true)
		return
	}

	// A started handler is not interrupted by shutdown, only by its own timeout.
	hctx, cancel := context.WithTimeout(context.WithoutCancel(session), c.cfg.HandlerTimeout)
	defer cancel()

	queue := c.topology.Queue
	start := time.Now()
	err := safeCall(logger.WithContext(hctx, log), handler, Delivery{
		MessageID:   d.MessageId,
		RoutingKey:  routingKey,
		Body:        d.Body,
		Redelivered: d.Redelivered,
		Retry:       retry,
	})
	processingDuration.WithLabelValues(queue, routingKey).Observe(time.Since(start).Seconds())

	var result string
	switch {
	case err == nil:
		result = resultAck
		if err := d.Ack(false); err != nil {
			log.Error("ack message", "error", err)
		}
	case IsPermanent(err):
		log.Error("message rejected, moving it to the dead letter queue", "error", err)
		result = c.forward(session, fwd, d, DeadLetterQueue(queue), deadLetterHeaders(d, routingKey, queue, retry, err), resultDeadLetter, log)
	case retry >= len(c.cfg.RetryDelays):
		log.Error("retries exhausted, moving the message to the dead letter queue", "error", err)
		result = c.forward(session, fwd, d, DeadLetterQueue(queue), deadLetterHeaders(d, routingKey, queue, retry, err), resultDeadLetter, log)
	default:
		delay := c.cfg.RetryDelays[retry]
		log.Warn("message processing failed, will retry", "error", err, "retry_in", delay.String())
		result = c.forward(session, fwd, d, RetryQueue(queue, delay), retryHeaders(d, routingKey, retry+1, err), resultRetry, log)
	}
	consumedMessages.WithLabelValues(queue, routingKey, result).Inc()
}

// forward publishes a copy of the message to a retry queue or the DLQ and then acks the
// original. A crash in between leaves both, so the message is handled twice: acceptable,
// because handlers are idempotent. Losing the message would not be.
// It returns result on success and resultRequeue if the copy could not be published.
func (c *Consumer) forward(session context.Context, fwd *forwarder, d amqp.Delivery, queue string, headers amqp.Table, result string, log *slog.Logger) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(session), forwardTimeout)
	err := fwd.publish(ctx, queue, amqp.Publishing{
		Headers:      headers,
		ContentType:  d.ContentType,
		DeliveryMode: amqp.Persistent,
		MessageId:    d.MessageId,
		Type:         d.Type,
		Timestamp:    d.Timestamp,
		AppId:        d.AppId,
		Body:         d.Body,
	})
	cancel()
	if err != nil {
		log.Error("forward message failed, requeueing it", "target_queue", queue, "error", err)
		select {
		case <-session.Done():
		case <-time.After(c.cfg.RequeueDelay):
		}
		nack(log, d, true)
		return resultRequeue
	}
	if err := d.Ack(false); err != nil {
		log.Error("ack message", "error", err)
	}
	return result
}

// safeCall turns a handler panic into an error, so one bad message cannot crash the service.
func safeCall(ctx context.Context, handler Handler, d Delivery) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Error("handler panicked", "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return handler(ctx, d)
}

func nack(log *slog.Logger, d amqp.Delivery, requeue bool) {
	if err := d.Nack(false, requeue); err != nil {
		log.Error("nack message", "error", err, "requeue", requeue)
	}
}

// forwarder publishes to retry queues and the DLQ on its own confirm-mode channel,
// shared by the consumer's workers.
type forwarder struct {
	mu sync.Mutex
	ch *amqp.Channel
}

func newForwarder(conn *amqp.Connection) (*forwarder, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open forwarding channel: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	return &forwarder{ch: ch}, nil
}

func (f *forwarder) publish(ctx context.Context, queue string, msg amqp.Publishing) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	confirm, err := f.ch.PublishWithDeferredConfirmWithContext(ctx, "", queue, false, false, msg)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	acked, err := confirm.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("wait for confirm: %w", err)
	}
	if !acked {
		return fmt.Errorf("message %s was not acknowledged by the broker", msg.MessageId)
	}
	return nil
}
