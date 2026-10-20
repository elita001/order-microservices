// Package domain contains the payment service's business model and errors.
package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

// Payment is a charge for one order. Amount is in minor currency units.
//
// A payment is created as pending before the provider is called, so its ID can serve as
// the provider's idempotency key: if processing is interrupted and the message is
// delivered again, the provider is called with the same key and returns the same result.
type Payment struct {
	ID            uuid.UUID
	OrderID       uuid.UUID
	CustomerID    uuid.UUID
	Amount        int64
	Currency      string
	Status        Status
	FailureReason string
	TransactionID string // provider's transaction ID
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

var ErrAlreadyCompleted = errors.New("payment is already completed")

// Succeed marks a pending payment as succeeded and returns the event to publish.
func (p *Payment) Succeed(transactionID string, at time.Time) (Event, error) {
	if p.Status != StatusPending {
		return nil, ErrAlreadyCompleted
	}
	p.Status = StatusSucceeded
	p.TransactionID = transactionID
	p.UpdatedAt = at
	return PaymentSucceeded{Payment: *p}, nil
}

// Fail marks a pending payment as failed and returns the event to publish.
func (p *Payment) Fail(reason, transactionID string, at time.Time) (Event, error) {
	if p.Status != StatusPending {
		return nil, ErrAlreadyCompleted
	}
	p.Status = StatusFailed
	p.FailureReason = reason
	p.TransactionID = transactionID
	p.UpdatedAt = at
	return PaymentFailed{Payment: *p}, nil
}

// ValidationError represents invalid input that retrying cannot fix.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}
