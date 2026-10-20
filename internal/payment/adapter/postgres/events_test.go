package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/payment/domain"
	"github.com/elita001/order-microservices/pkg/events"
)

func TestToOutboxMessage(t *testing.T) {
	payment := domain.Payment{
		ID:            uuid.New(),
		OrderID:       uuid.New(),
		CustomerID:    uuid.New(),
		Amount:        119800,
		Currency:      "RUB",
		FailureReason: "card_declined",
		UpdatedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	tests := []struct {
		event      domain.Event
		wantType   string
		wantFields []string
	}{
		{domain.PaymentSucceeded{Payment: payment}, events.PaymentSucceededType,
			[]string{"payment_id", "order_id", "customer_id", "amount", "currency", "transaction_id", "processed_at"}},
		{domain.PaymentFailed{Payment: payment}, events.PaymentFailedType,
			[]string{"payment_id", "order_id", "customer_id", "amount", "currency", "reason", "processed_at"}},
	}
	for _, tt := range tests {
		t.Run(tt.wantType, func(t *testing.T) {
			msg, err := toOutboxMessage(tt.event)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if msg.EventType != tt.wantType || msg.AggregateType != events.AggregatePayment {
				t.Errorf("type = %s/%s, want %s/%s", msg.AggregateType, msg.EventType, events.AggregatePayment, tt.wantType)
			}
			if msg.AggregateID != payment.ID.String() {
				t.Errorf("aggregate id = %s, want %s", msg.AggregateID, payment.ID)
			}

			// The JSON field names are the contract with other services.
			var payload map[string]any
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				t.Fatalf("payload is not JSON: %v", err)
			}
			for _, field := range tt.wantFields {
				if _, ok := payload[field]; !ok {
					t.Errorf("payload has no %q field: %s", field, msg.Payload)
				}
			}
		})
	}
}
