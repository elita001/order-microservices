package rabbitmq

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/elita001/order-microservices/pkg/outbox"
)

// Publisher publishes outbox messages to a topic exchange using the event type as the
// routing key. Each Publish waits for the broker confirm, so a nil error means the
// message is safely stored by RabbitMQ.
//
// The connection is opened lazily and reopened on the next Publish after any failure,
// so the caller only has to retry (the outbox relay does this with backoff).
type Publisher struct {
	url      string
	exchange string
	appID    string
	log      *slog.Logger

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

var _ outbox.Publisher = (*Publisher)(nil)

func NewPublisher(url, exchange, appID string, log *slog.Logger) *Publisher {
	return &Publisher{url: url, exchange: exchange, appID: appID, log: log.With("component", "rabbitmq_publisher")}
}

func (p *Publisher) Publish(ctx context.Context, msg outbox.Message) (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Any failure may leave the channel in an unknown state: drop it and reconnect next time.
	defer func() {
		if err != nil {
			p.reset()
		}
	}()

	ch, err := p.channel()
	if err != nil {
		return err
	}

	// mandatory=false: until consumers bind their queues (next steps), unroutable
	// messages are dropped by the broker instead of being returned.
	confirm, err := ch.PublishWithDeferredConfirmWithContext(ctx, p.exchange, msg.EventType, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    msg.ID.String(),
		Type:         msg.EventType,
		Timestamp:    msg.CreatedAt,
		AppId:        p.appID,
		Headers: amqp.Table{
			"aggregate_type": msg.AggregateType,
			"aggregate_id":   msg.AggregateID,
		},
		Body: msg.Payload,
	})
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	acked, err := confirm.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("wait for confirm: %w", err)
	}
	if !acked {
		return fmt.Errorf("message %s was not acknowledged by the broker", msg.ID)
	}
	return nil
}

// Close closes the channel and connection. It is safe to call more than once.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reset()
}

func (p *Publisher) channel() (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	p.reset()

	conn, err := Dial(p.url, p.appID)
	if err != nil {
		return nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open channel: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	if err := DeclareExchange(ch, p.exchange); err != nil {
		_ = conn.Close()
		return nil, err
	}

	p.conn, p.ch = conn, ch
	p.log.Info("connected to rabbitmq", "exchange", p.exchange)
	return ch, nil
}

func (p *Publisher) reset() {
	if p.ch != nil {
		_ = p.ch.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.ch, p.conn = nil, nil
}
