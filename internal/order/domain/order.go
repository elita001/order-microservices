// Package domain contains the order service's business model and errors.
package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending       Status = "pending"
	StatusPaid          Status = "paid"
	StatusPaymentFailed Status = "payment_failed"
	StatusShipped       Status = "shipped"
)

// Item is an order line. Price is the per-unit price in minor currency units.
type Item struct {
	ProductID string
	Quantity  int
	Price     int64
}

type Order struct {
	ID          uuid.UUID
	CustomerID  uuid.UUID
	Status      Status
	Items       []Item
	TotalAmount int64
	Currency    string
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// IdempotencyKey is the client's Idempotency-Key header (empty if not sent), and
	// RequestHash identifies the request body, so a reused key with another body is rejected.
	IdempotencyKey string
	RequestHash    string
}

var ErrNotFound = errors.New("order not found")

// ErrForbidden means the viewer may not access another customer's data.
var ErrForbidden = errors.New("access denied")

// ErrDuplicateIdempotencyKey is returned by the repository when an order with the same
// idempotency key already exists (e.g., created by a concurrent request).
var ErrDuplicateIdempotencyKey = errors.New("order with this idempotency key already exists")

// ErrIdempotencyKeyReused means the client sent an idempotency key that was already
// used for a request with a different body.
var ErrIdempotencyKeyReused = errors.New("idempotency key was already used for a different request")

// ValidationError represents an input error that can be shown to the client.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}
