package domain

import (
	"time"

	"github.com/google/uuid"
)

// Event is a fact about a change in the order aggregate that other services may react to.
// Events are persisted atomically with the order and published via the transactional outbox.
type Event interface {
	isEvent()
}

// OrderCreated is raised when a new order is placed and awaits payment.
type OrderCreated struct {
	OrderID     uuid.UUID
	CustomerID  uuid.UUID
	Items       []Item
	TotalAmount int64
	Currency    string
	CreatedAt   time.Time
}

func (OrderCreated) isEvent() {}

func NewOrderCreated(order *Order) OrderCreated {
	return OrderCreated{
		OrderID:     order.ID,
		CustomerID:  order.CustomerID,
		Items:       order.Items,
		TotalAmount: order.TotalAmount,
		Currency:    order.Currency,
		CreatedAt:   order.CreatedAt,
	}
}
