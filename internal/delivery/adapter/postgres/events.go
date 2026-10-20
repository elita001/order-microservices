package postgres

import (
	"fmt"

	"github.com/elita001/order-microservices/internal/delivery/domain"
	"github.com/elita001/order-microservices/pkg/events"
	"github.com/elita001/order-microservices/pkg/outbox"
)

// toOutboxMessages maps domain events to integration event contracts.
func toOutboxMessages(domainEvents []domain.Event) ([]outbox.Message, error) {
	msgs := make([]outbox.Message, 0, len(domainEvents))
	for _, event := range domainEvents {
		msg, err := toOutboxMessage(event)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

func toOutboxMessage(event domain.Event) (outbox.Message, error) {
	switch e := event.(type) {
	case domain.ShipmentRegistered:
		s := e.Shipment
		return outbox.NewMessage(events.AggregateShipment, s.ID.String(), events.DeliveryCreatedType, events.DeliveryCreated{
			ShipmentID:          s.ID,
			OrderID:             s.OrderID,
			CustomerID:          s.CustomerID,
			Carrier:             s.Carrier,
			TrackingNumber:      s.TrackingNumber,
			EstimatedDeliveryAt: s.EstimatedDeliveryAt,
			CreatedAt:           s.UpdatedAt,
		})
	default:
		return outbox.Message{}, fmt.Errorf("unsupported domain event %T", event)
	}
}
