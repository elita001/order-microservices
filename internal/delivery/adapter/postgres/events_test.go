package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/delivery/domain"
	"github.com/elita001/order-microservices/pkg/events"
)

func TestToOutboxMessage_ShipmentRegistered(t *testing.T) {
	shipment := domain.Shipment{
		ID:                  uuid.New(),
		OrderID:             uuid.New(),
		CustomerID:          uuid.New(),
		Status:              domain.StatusRegistered,
		Carrier:             "CDEK",
		TrackingNumber:      "OF0123456789",
		EstimatedDeliveryAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		UpdatedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	msg, err := toOutboxMessage(domain.ShipmentRegistered{Shipment: shipment})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.EventType != events.DeliveryCreatedType || msg.AggregateType != events.AggregateShipment {
		t.Errorf("type = %s/%s, want %s/%s", msg.AggregateType, msg.EventType, events.AggregateShipment, events.DeliveryCreatedType)
	}

	// The JSON field names are the contract with other services.
	var payload map[string]any
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	for _, field := range []string{"shipment_id", "order_id", "customer_id", "carrier", "tracking_number", "estimated_delivery_at", "created_at"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("payload has no %q field: %s", field, msg.Payload)
		}
	}
}
