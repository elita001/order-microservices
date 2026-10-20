// Package domain contains the notification service's business model and errors.
package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kind identifies the reason for a notification.
type Kind string

const (
	KindOrderCreated     Kind = "order_created"
	KindPaymentSucceeded Kind = "payment_succeeded"
	KindPaymentFailed    Kind = "payment_failed"
	KindOrderShipped     Kind = "order_shipped"
)

// Notification is a sent email. A customer gets at most one notification
// of each kind per order, even if the triggering event is delivered twice.
type Notification struct {
	ID         uuid.UUID
	Kind       Kind
	OrderID    uuid.UUID
	CustomerID uuid.UUID
	Recipient  string
	Subject    string
	SentAt     time.Time
}

// ValidationError represents invalid input that retrying cannot fix.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}
