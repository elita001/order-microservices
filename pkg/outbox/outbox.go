// Package outbox implements the transactional outbox pattern: events are stored in the
// same database transaction as the business data and published to the broker by a
// background relay. Delivery is at-least-once, so consumers must deduplicate by message ID.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Message is an event waiting to be published.
type Message struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte // JSON
	CreatedAt     time.Time
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

// Store inserts messages using the caller's transaction, so they are committed or
// rolled back together with the business data.
func Store(ctx context.Context, tx pgx.Tx, msgs ...Message) error {
	if len(msgs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, msg := range msgs {
		batch.Queue(`
			INSERT INTO outbox (id, aggregate_type, aggregate_id, event_type, payload, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			msg.ID, msg.AggregateType, msg.AggregateID, msg.EventType, msg.Payload, msg.CreatedAt)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("insert outbox messages: %w", err)
	}
	return nil
}
