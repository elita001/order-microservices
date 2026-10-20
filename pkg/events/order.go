// Package events defines integration event contracts exchanged between services.
// Changing a field here changes the wire format: add fields, never rename or remove them.
package events

import (
	"time"

	"github.com/google/uuid"
)

// AggregateOrder is the aggregate type of order events.
const AggregateOrder = "order"

// OrderCreatedType is both the event type and the RabbitMQ routing key.
const OrderCreatedType = "order.created"

type OrderItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
	Price     int64  `json:"price"`
}

// OrderCreated is published when a new order is placed and awaits payment.
// Amounts are in minor currency units.
type OrderCreated struct {
	OrderID     uuid.UUID   `json:"order_id"`
	CustomerID  uuid.UUID   `json:"customer_id"`
	Items       []OrderItem `json:"items"`
	TotalAmount int64       `json:"total_amount"`
	Currency    string      `json:"currency"`
	CreatedAt   time.Time   `json:"created_at"`
}
