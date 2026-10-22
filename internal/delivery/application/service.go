// Package application implements delivery use cases and defines their ports.
package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/delivery/domain"
)

// Repository is the persistence port required by the delivery use cases.
type Repository interface {
	// GetOrCreate returns the shipment for the order, creating the given pending
	// shipment if the order has none. One order never gets two shipments.
	GetOrCreate(ctx context.Context, shipment *domain.Shipment) (*domain.Shipment, error)
	// Complete saves a registered shipment together with its events in one transaction.
	// It returns false if the shipment was already registered.
	Complete(ctx context.Context, shipment *domain.Shipment, events ...domain.Event) (bool, error)
}

// Carrier is the port of the external delivery provider.
type Carrier interface {
	// RegisterShipment returns an error for technical failures; the call is safe to retry
	// with the same idempotency key.
	RegisterShipment(ctx context.Context, req RegisterShipmentRequest) (RegisterShipmentResult, error)
}

type RegisterShipmentRequest struct {
	IdempotencyKey string
	OrderID        uuid.UUID
	CustomerID     uuid.UUID
}

type RegisterShipmentResult struct {
	Carrier             string
	TrackingNumber      string
	EstimatedDeliveryAt time.Time
}

// Logger is the logging capability required by the application service.
// It is called with the request context, so the records carry request_id and trace_id.
type Logger interface {
	InfoContext(ctx context.Context, message string, args ...any)
}

// Metrics records business metrics; a Prometheus adapter implements it.
type Metrics interface {
	ShipmentRegistered(carrier string)
}

type Service struct {
	repo    Repository
	carrier Carrier
	logger  Logger
	metrics Metrics
	now     func() time.Time
}

func NewService(repo Repository, carrier Carrier, logger Logger) *Service {
	return &Service{
		repo:    repo,
		carrier: carrier,
		logger:  logger,
		metrics: noopMetrics{},
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithMetrics enables business metrics; without it they are not recorded.
func (s *Service) WithMetrics(m Metrics) *Service {
	s.metrics = m
	return s
}

type noopMetrics struct{}

func (noopMetrics) ShipmentRegistered(string) {}

type ShipOrderInput struct {
	OrderID    uuid.UUID
	CustomerID uuid.UUID
}

// ShipOrder hands a paid order over to the carrier.
// It is idempotent: a repeated call for the same order does not register a second parcel.
func (s *Service) ShipOrder(ctx context.Context, in ShipOrderInput) error {
	if in.OrderID == uuid.Nil {
		return &domain.ValidationError{Field: "order_id", Message: "is required"}
	}
	if in.CustomerID == uuid.Nil {
		return &domain.ValidationError{Field: "customer_id", Message: "is required"}
	}

	now := s.now()
	shipment, err := s.repo.GetOrCreate(ctx, &domain.Shipment{
		ID:         uuid.New(),
		OrderID:    in.OrderID,
		CustomerID: in.CustomerID,
		Status:     domain.StatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		return fmt.Errorf("get or create shipment for order %s: %w", in.OrderID, err)
	}
	if shipment.Status != domain.StatusPending {
		s.logger.InfoContext(ctx, "shipment already registered, skipping",
			"shipment_id", shipment.ID, "order_id", shipment.OrderID)
		return nil
	}

	result, err := s.carrier.RegisterShipment(ctx, RegisterShipmentRequest{
		IdempotencyKey: shipment.ID.String(),
		OrderID:        shipment.OrderID,
		CustomerID:     shipment.CustomerID,
	})
	if err != nil {
		return fmt.Errorf("register shipment %s with carrier: %w", shipment.ID, err)
	}

	event, err := shipment.Register(result.Carrier, result.TrackingNumber, result.EstimatedDeliveryAt, s.now())
	if err != nil {
		return fmt.Errorf("register shipment %s: %w", shipment.ID, err)
	}
	completed, err := s.repo.Complete(ctx, shipment, event)
	if err != nil {
		return fmt.Errorf("save shipment %s: %w", shipment.ID, err)
	}
	if !completed {
		// Another delivery of the same message registered the shipment concurrently.
		s.logger.InfoContext(ctx, "shipment registered concurrently, skipping", "shipment_id", shipment.ID, "order_id", shipment.OrderID)
		return nil
	}

	s.metrics.ShipmentRegistered(shipment.Carrier)
	s.logger.InfoContext(ctx, "shipment registered",
		"shipment_id", shipment.ID,
		"order_id", shipment.OrderID,
		"carrier", shipment.Carrier,
		"tracking_number", shipment.TrackingNumber,
	)
	return nil
}

// IsValidationError reports whether err is caused by invalid input.
func IsValidationError(err error) bool {
	var validationErr *domain.ValidationError
	return errors.As(err, &validationErr)
}
