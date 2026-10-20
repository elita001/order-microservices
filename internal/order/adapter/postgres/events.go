package postgres

import (
	"fmt"

	"github.com/elita001/order-microservices/internal/order/domain"
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
	case domain.OrderCreated:
		items := make([]events.OrderItem, 0, len(e.Items))
		for _, item := range e.Items {
			items = append(items, events.OrderItem{ProductID: item.ProductID, Quantity: item.Quantity, Price: item.Price})
		}
		return outbox.NewMessage(events.AggregateOrder, e.OrderID.String(), events.OrderCreatedType, events.OrderCreated{
			OrderID:     e.OrderID,
			CustomerID:  e.CustomerID,
			Items:       items,
			TotalAmount: e.TotalAmount,
			Currency:    e.Currency,
			CreatedAt:   e.CreatedAt,
		})
	default:
		return outbox.Message{}, fmt.Errorf("unsupported domain event %T", event)
	}
}
