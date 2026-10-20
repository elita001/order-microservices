// Package domain contains the delivery service's business model and errors.
package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusRegistered Status = "registered" // handed over to the carrier
)

// Shipment is the delivery of one paid order.
//
// A shipment is created as pending before the carrier is called, so its ID can serve as
// the carrier's idempotency key: a redelivered event never registers a second parcel.
type Shipment struct {
	ID                  uuid.UUID
	OrderID             uuid.UUID
	CustomerID          uuid.UUID
	Status              Status
	Carrier             string
	TrackingNumber      string
	EstimatedDeliveryAt time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

var ErrAlreadyRegistered = errors.New("shipment is already registered")

// Register marks a pending shipment as handed over to the carrier and returns the event to publish.
func (s *Shipment) Register(carrier, trackingNumber string, estimatedDeliveryAt, at time.Time) (Event, error) {
	if s.Status != StatusPending {
		return nil, ErrAlreadyRegistered
	}
	s.Status = StatusRegistered
	s.Carrier = carrier
	s.TrackingNumber = trackingNumber
	s.EstimatedDeliveryAt = estimatedDeliveryAt
	s.UpdatedAt = at
	return ShipmentRegistered{Shipment: *s}, nil
}

// ValidationError represents invalid input that retrying cannot fix.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}
