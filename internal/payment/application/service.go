// Package application implements payment use cases and defines their ports.
package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/payment/domain"
)

// Repository is the persistence port required by the payment use cases.
type Repository interface {
	// GetOrCreate returns the payment for the order, creating the given pending
	// payment if the order has none. One order never gets two payments.
	GetOrCreate(ctx context.Context, payment *domain.Payment) (*domain.Payment, error)
	// Complete saves the final status of a pending payment together with its events
	// in one transaction. It returns false if the payment was already completed.
	Complete(ctx context.Context, payment *domain.Payment, events ...domain.Event) (bool, error)
}

// Provider is the port of the external payment provider.
type Provider interface {
	// Charge returns an error only for technical failures (timeouts, unavailability).
	// A declined payment is a successful call with Approved = false.
	Charge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

type ChargeRequest struct {
	// IdempotencyKey makes repeated calls for the same payment return the same result
	// instead of charging the customer twice.
	IdempotencyKey string
	CustomerID     uuid.UUID
	Amount         int64
	Currency       string
}

type ChargeResult struct {
	Approved      bool
	DeclineReason string
	TransactionID string
}

// Logger is the logging capability required by the application service.
type Logger interface {
	Info(message string, args ...any)
}

type Service struct {
	repo     Repository
	provider Provider
	logger   Logger
	now      func() time.Time
}

func NewService(repo Repository, provider Provider, logger Logger) *Service {
	return &Service{
		repo:     repo,
		provider: provider,
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

type ProcessPaymentInput struct {
	OrderID    uuid.UUID
	CustomerID uuid.UUID
	Amount     int64
	Currency   string
}

// ProcessPayment charges the customer for an order and records the result.
// It is idempotent: a repeated call for the same order does not charge again.
func (s *Service) ProcessPayment(ctx context.Context, in ProcessPaymentInput) error {
	if err := validate(in); err != nil {
		return err
	}

	now := s.now()
	payment, err := s.repo.GetOrCreate(ctx, &domain.Payment{
		ID:         uuid.New(),
		OrderID:    in.OrderID,
		CustomerID: in.CustomerID,
		Amount:     in.Amount,
		Currency:   in.Currency,
		Status:     domain.StatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		return fmt.Errorf("get or create payment for order %s: %w", in.OrderID, err)
	}
	if payment.Status != domain.StatusPending {
		s.logger.Info("payment already processed, skipping",
			"payment_id", payment.ID, "order_id", payment.OrderID, "status", payment.Status)
		return nil
	}

	result, err := s.provider.Charge(ctx, ChargeRequest{
		IdempotencyKey: payment.ID.String(),
		CustomerID:     payment.CustomerID,
		Amount:         payment.Amount,
		Currency:       payment.Currency,
	})
	if err != nil {
		return fmt.Errorf("charge payment %s: %w", payment.ID, err)
	}

	var event domain.Event
	if result.Approved {
		event, err = payment.Succeed(result.TransactionID, s.now())
	} else {
		event, err = payment.Fail(result.DeclineReason, result.TransactionID, s.now())
	}
	if err != nil {
		return fmt.Errorf("complete payment %s: %w", payment.ID, err)
	}

	completed, err := s.repo.Complete(ctx, payment, event)
	if err != nil {
		return fmt.Errorf("save payment %s: %w", payment.ID, err)
	}
	if !completed {
		// Another delivery of the same message completed the payment concurrently.
		s.logger.Info("payment completed concurrently, skipping", "payment_id", payment.ID, "order_id", payment.OrderID)
		return nil
	}

	s.logger.Info("payment processed",
		"payment_id", payment.ID,
		"order_id", payment.OrderID,
		"status", payment.Status,
		"amount", payment.Amount,
		"currency", payment.Currency,
		"reason", payment.FailureReason,
	)
	return nil
}

func validate(in ProcessPaymentInput) error {
	switch {
	case in.OrderID == uuid.Nil:
		return &domain.ValidationError{Field: "order_id", Message: "is required"}
	case in.CustomerID == uuid.Nil:
		return &domain.ValidationError{Field: "customer_id", Message: "is required"}
	case in.Amount <= 0:
		return &domain.ValidationError{Field: "amount", Message: "must be positive"}
	case len(in.Currency) != 3:
		return &domain.ValidationError{Field: "currency", Message: "must be a 3-letter ISO code"}
	}
	return nil
}

// IsValidationError reports whether err is caused by invalid input.
func IsValidationError(err error) bool {
	var validationErr *domain.ValidationError
	return errors.As(err, &validationErr)
}
