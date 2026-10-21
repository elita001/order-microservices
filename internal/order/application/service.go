// Package application implements order use cases and defines their ports.
package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/order/domain"
)

const (
	maxItems         = 100
	maxQuantity      = 1000
	maxPrice         = 1_000_000_000_000
	defaultListLimit = 20
	maxListLimit     = 100

	maxIdempotencyKeyLength = 255
)

var supportedCurrencies = map[string]struct{}{"RUB": {}, "USD": {}, "EUR": {}}

// Repository is the persistence port required by the order use cases.
type Repository interface {
	// Create persists the order together with its events in one transaction,
	// so an event is published if and only if the order is saved.
	Create(ctx context.Context, order *domain.Order, events ...domain.Event) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Order, error)
	// GetByIdempotencyKey returns domain.ErrNotFound if no order has this key.
	GetByIdempotencyKey(ctx context.Context, key string) (*domain.Order, error)
	ListByCustomer(ctx context.Context, customerID uuid.UUID, limit, offset int) ([]domain.Order, error)
	// UpdateStatus changes the status only if the order is currently in one of the from
	// statuses and reports whether it was changed.
	UpdateStatus(ctx context.Context, id uuid.UUID, from []domain.Status, to domain.Status, updatedAt time.Time) (bool, error)
}

// Logger is the logging capability required by the application service.
type Logger interface {
	Info(message string, args ...any)
}

type Service struct {
	repo   Repository
	logger Logger
	now    func() time.Time
}

func NewService(repo Repository, logger Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

type CreateOrderInput struct {
	CustomerID     uuid.UUID
	Currency       string
	Items          []domain.Item
	IdempotencyKey string // optional
}

type CreateOrderResult struct {
	Order *domain.Order
	// Replayed is true when the order was created earlier by a request with the same
	// idempotency key and is returned again instead of creating a duplicate.
	Replayed bool
}

type ListOrdersInput struct {
	CustomerID uuid.UUID
	Limit      int
	Offset     int
}

type ListOrdersResult struct {
	Orders []domain.Order
	Limit  int
	Offset int
}

type CompletePaymentInput struct {
	OrderID   uuid.UUID
	Succeeded bool
	Reason    string // why the payment failed; empty on success
}

// Create places a new order. With an idempotency key, repeating the request returns the
// order created by the first one, so a client can safely retry after a timeout.
func (s *Service) Create(ctx context.Context, in CreateOrderInput) (*CreateOrderResult, error) {
	if err := validate(in); err != nil {
		return nil, err
	}

	hash := requestHash(in)
	if in.IdempotencyKey != "" {
		result, err := s.replay(ctx, in.IdempotencyKey, hash)
		if result != nil || err != nil {
			return result, err
		}
	}

	var total int64
	for _, item := range in.Items {
		total += item.Price * int64(item.Quantity)
	}

	now := s.now()
	order := &domain.Order{
		ID:          uuid.New(),
		CustomerID:  in.CustomerID,
		Status:      domain.StatusPending,
		Items:       in.Items,
		TotalAmount: total,
		Currency:    in.Currency,
		CreatedAt:   now,
		UpdatedAt:   now,

		IdempotencyKey: in.IdempotencyKey,
		RequestHash:    hash,
	}

	err := s.repo.Create(ctx, order, domain.NewOrderCreated(order))
	if errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		// A concurrent request with the same key has created the order first.
		result, err := s.replay(ctx, in.IdempotencyKey, hash)
		if result == nil && err == nil {
			err = fmt.Errorf("order with idempotency key %q disappeared", in.IdempotencyKey)
		}
		return result, err
	}
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	s.logger.Info("order created",
		"order_id", order.ID,
		"customer_id", order.CustomerID,
		"total_amount", order.TotalAmount,
		"currency", order.Currency,
	)
	return &CreateOrderResult{Order: order}, nil
}

// replay returns the order created earlier with the same idempotency key, or nil if there is none.
func (s *Service) replay(ctx context.Context, key, hash string) (*CreateOrderResult, error) {
	existing, err := s.repo.GetByIdempotencyKey(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get order by idempotency key: %w", err)
	}
	if existing.RequestHash != hash {
		return nil, domain.ErrIdempotencyKeyReused
	}
	s.logger.Info("order request replayed", "order_id", existing.ID)
	return &CreateOrderResult{Order: existing, Replayed: true}, nil
}

// requestHash identifies the order request: the same items in the same order give the same hash.
func requestHash(in CreateOrderInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s", in.CustomerID, in.Currency)
	for _, item := range in.Items {
		fmt.Fprintf(h, "|%q:%d:%d", item.ProductID, item.Quantity, item.Price)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*domain.Order, error) {
	order, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get order %s: %w", id, err)
	}
	return order, nil
}

func (s *Service) ListByCustomer(ctx context.Context, in ListOrdersInput) (*ListOrdersResult, error) {
	if in.CustomerID == uuid.Nil {
		return nil, &domain.ValidationError{Field: "customer_id", Message: "is required"}
	}
	if in.Offset < 0 {
		return nil, &domain.ValidationError{Field: "offset", Message: "must not be negative"}
	}
	limit, err := normalizeLimit(in.Limit)
	if err != nil {
		return nil, err
	}
	orders, err := s.repo.ListByCustomer(ctx, in.CustomerID, limit, in.Offset)
	if err != nil {
		return nil, fmt.Errorf("list orders for customer %s: %w", in.CustomerID, err)
	}
	return &ListOrdersResult{Orders: orders, Limit: limit, Offset: in.Offset}, nil
}

// CompletePayment moves a pending order to paid or payment_failed.
// Repeated or late events do not change the order, which makes the event handler idempotent.
func (s *Service) CompletePayment(ctx context.Context, in CompletePaymentInput) error {
	if in.OrderID == uuid.Nil {
		return &domain.ValidationError{Field: "order_id", Message: "is required"}
	}
	to := domain.StatusPaymentFailed
	if in.Succeeded {
		to = domain.StatusPaid
	}
	return s.changeStatus(ctx, in.OrderID, []domain.Status{domain.StatusPending}, to, in.Reason)
}

// MarkShipped moves a paid order to shipped.
//
// Events from different services may arrive out of order: the delivery event can be
// handled before the payment event. A shipment implies a successful payment, so a pending
// order is moved to shipped as well, and the late payment event then changes nothing.
func (s *Service) MarkShipped(ctx context.Context, orderID uuid.UUID) error {
	if orderID == uuid.Nil {
		return &domain.ValidationError{Field: "order_id", Message: "is required"}
	}
	return s.changeStatus(ctx, orderID, []domain.Status{domain.StatusPending, domain.StatusPaid}, domain.StatusShipped, "")
}

func (s *Service) changeStatus(ctx context.Context, orderID uuid.UUID, from []domain.Status, to domain.Status, reason string) error {
	updated, err := s.repo.UpdateStatus(ctx, orderID, from, to, s.now())
	if err != nil {
		return fmt.Errorf("change status of order %s to %s: %w", orderID, to, err)
	}
	if !updated {
		s.logger.Info("order status not changed: order is in another status or does not exist",
			"order_id", orderID, "target_status", to)
		return nil
	}

	s.logger.Info("order status changed",
		"order_id", orderID,
		"status", to,
		"reason", reason,
	)
	return nil
}

func validate(in CreateOrderInput) error {
	if len(in.IdempotencyKey) > maxIdempotencyKeyLength {
		return &domain.ValidationError{Field: "Idempotency-Key", Message: fmt.Sprintf("must be at most %d characters", maxIdempotencyKeyLength)}
	}
	if in.CustomerID == uuid.Nil {
		return &domain.ValidationError{Field: "customer_id", Message: "is required"}
	}
	if _, ok := supportedCurrencies[in.Currency]; !ok {
		return &domain.ValidationError{Field: "currency", Message: "must be one of RUB, USD, EUR"}
	}
	if len(in.Items) == 0 || len(in.Items) > maxItems {
		return &domain.ValidationError{Field: "items", Message: fmt.Sprintf("must contain 1..%d items", maxItems)}
	}
	for i, item := range in.Items {
		field := fmt.Sprintf("items[%d]", i)
		switch {
		case item.ProductID == "":
			return &domain.ValidationError{Field: field + ".product_id", Message: "is required"}
		case item.Quantity < 1 || item.Quantity > maxQuantity:
			return &domain.ValidationError{Field: field + ".quantity", Message: fmt.Sprintf("must be in 1..%d", maxQuantity)}
		case item.Price <= 0 || item.Price > maxPrice:
			return &domain.ValidationError{Field: field + ".price", Message: "must be positive and not too large"}
		}
	}
	return nil
}

func normalizeLimit(limit int) (int, error) {
	switch {
	case limit < 0:
		return 0, &domain.ValidationError{Field: "limit", Message: "must not be negative"}
	case limit == 0:
		return defaultListLimit, nil
	case limit > maxListLimit:
		return maxListLimit, nil
	default:
		return limit, nil
	}
}
