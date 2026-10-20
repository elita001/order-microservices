package postgres

import (
	"fmt"

	"github.com/elita001/order-microservices/internal/payment/domain"
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
	case domain.PaymentSucceeded:
		p := e.Payment
		return outbox.NewMessage(events.AggregatePayment, p.ID.String(), events.PaymentSucceededType, events.PaymentSucceeded{
			PaymentID:     p.ID,
			OrderID:       p.OrderID,
			CustomerID:    p.CustomerID,
			Amount:        p.Amount,
			Currency:      p.Currency,
			TransactionID: p.TransactionID,
			ProcessedAt:   p.UpdatedAt,
		})
	case domain.PaymentFailed:
		p := e.Payment
		return outbox.NewMessage(events.AggregatePayment, p.ID.String(), events.PaymentFailedType, events.PaymentFailed{
			PaymentID:   p.ID,
			OrderID:     p.OrderID,
			CustomerID:  p.CustomerID,
			Amount:      p.Amount,
			Currency:    p.Currency,
			Reason:      p.FailureReason,
			ProcessedAt: p.UpdatedAt,
		})
	default:
		return outbox.Message{}, fmt.Errorf("unsupported domain event %T", event)
	}
}
