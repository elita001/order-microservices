package events

import (
	"time"

	"github.com/google/uuid"
)

// AggregatePayment is the aggregate type of payment events.
const AggregatePayment = "payment"

// Payment event types, also used as RabbitMQ routing keys.
const (
	PaymentSucceededType = "payment.succeeded"
	PaymentFailedType    = "payment.failed"
)

// PaymentSucceeded is published when the provider has charged the customer for an order.
type PaymentSucceeded struct {
	PaymentID     uuid.UUID `json:"payment_id"`
	OrderID       uuid.UUID `json:"order_id"`
	CustomerID    uuid.UUID `json:"customer_id"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	TransactionID string    `json:"transaction_id"`
	ProcessedAt   time.Time `json:"processed_at"`
}

// PaymentFailed is published when the provider has declined the payment for an order.
type PaymentFailed struct {
	PaymentID   uuid.UUID `json:"payment_id"`
	OrderID     uuid.UUID `json:"order_id"`
	CustomerID  uuid.UUID `json:"customer_id"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Reason      string    `json:"reason"`
	ProcessedAt time.Time `json:"processed_at"`
}
