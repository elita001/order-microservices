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
}

var ErrNotFound = errors.New("order not found")

// ValidationError represents an input error that can be shown to the client.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}
