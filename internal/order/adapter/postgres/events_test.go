package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/pkg/events"
)

func TestToOutboxMessage_OrderCreated(t *testing.T) {
	event := domain.OrderCreated{
		OrderID:     uuid.New(),
		CustomerID:  uuid.New(),
		Items:       []domain.Item{{ProductID: "book-42", Quantity: 2, Price: 59900}},
		TotalAmount: 119800,
		Currency:    "RUB",
		CreatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	msg, err := toOutboxMessage(event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.EventType != events.OrderCreatedType || msg.AggregateType != events.AggregateOrder {
		t.Errorf("type = %s/%s, want %s/%s", msg.AggregateType, msg.EventType, events.AggregateOrder, events.OrderCreatedType)
	}
	if msg.AggregateID != event.OrderID.String() {
		t.Errorf("aggregate id = %s, want %s", msg.AggregateID, event.OrderID)
	}
	if msg.ID == uuid.Nil {
		t.Error("message id is empty")
	}

	// The JSON field names are the contract with other services.
	var payload map[string]any
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	for _, field := range []string{"order_id", "customer_id", "items", "total_amount", "currency", "created_at"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("payload has no %q field: %s", field, msg.Payload)
		}
	}
}
