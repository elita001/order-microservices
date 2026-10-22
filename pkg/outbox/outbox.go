// Package outbox implements the transactional outbox pattern: events are stored in the
// same database transaction as the business data and published to the broker by a
// background relay. Delivery is at-least-once, so consumers must be idempotent.
//
// Every service owns its outbox table (order_outbox, payment_outbox) with the schema
// from the service's migrations; the table name is passed explicitly.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/propagation"

	"github.com/elita001/order-microservices/pkg/requestid"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

// Message is an event waiting to be published.
type Message struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte // JSON
	CreatedAt     time.Time
	// Headers carry the context of the operation that produced the event (trace context,
	// request ID) to the consumers. Store fills them from its context.
	Headers map[string]string
}

// NewMessage marshals payload to JSON. The ID is a time-ordered UUIDv7 and doubles as the
// broker message ID that consumers use for deduplication.
func NewMessage(aggregateType, aggregateID, eventType string, payload any) (Message, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Message{}, fmt.Errorf("generate message id: %w", err)
	}
	return Message{
		ID:            id,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		EventType:     eventType,
		Payload:       data,
		CreatedAt:     time.Now().UTC(),
	}, nil
}

// Store inserts messages into the service's outbox table using the caller's transaction,
// so they are committed or rolled back together with the business data. The trace context
// and the request ID of ctx are saved with each message.
func Store(ctx context.Context, tx pgx.Tx, table string, msgs ...Message) error {
	if len(msgs) == 0 {
		return nil
	}
	contextHeaders := HeadersFromContext(ctx)
	query := fmt.Sprintf(`
		INSERT INTO %s (id, aggregate_type, aggregate_id, event_type, payload, created_at, headers)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, pgx.Identifier{table}.Sanitize())
	batch := &pgx.Batch{}
	for _, msg := range msgs {
		headers := maps.Clone(contextHeaders)
		maps.Copy(headers, msg.Headers) // explicit headers win
		batch.Queue(query,
			msg.ID, msg.AggregateType, msg.AggregateID, msg.EventType, msg.Payload, msg.CreatedAt, headers)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("insert outbox messages: %w", err)
	}
	return nil
}

// HeadersFromContext captures the trace context (traceparent) and the request ID of ctx.
func HeadersFromContext(ctx context.Context) map[string]string {
	headers := map[string]string{}
	telemetry.Inject(ctx, propagation.MapCarrier(headers))
	if id := requestid.FromContext(ctx); id != "" {
		headers[requestid.MessageHeader] = id
	}
	return headers
}
