// Package application implements order use cases and defines their ports.
package application

import (
	"context"
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
)

var supportedCurrencies = map[string]struct{}{"RUB": {}, "USD": {}, "EUR": {}}

// Repository is the persistence port required by the order use cases.
type Repository interface {
	// Create persists the order together with its events in one transaction,
	// so an event is published if and only if the order is saved.
	Create(ctx context.Context, order *domain.Order, events ...domain.Event) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Order, error)
	ListByCustomer(ctx context.Context, customerID uuid.UUID, limit, offset int) ([]domain.Order, error)
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
	CustomerID uuid.UUID
	Currency   string
	Items      []domain.Item
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

func (s *Service) Create(ctx context.Context, in CreateOrderInput) (*domain.Order, error) {
	if err := validate(in); err != nil {
		return nil, err
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
	}

	if err := s.repo.Create(ctx, order, domain.NewOrderCreated(order)); err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	s.logger.Info("order created",
		"order_id", order.ID,
		"customer_id", order.CustomerID,
		"total_amount", order.TotalAmount,
		"currency", order.Currency,
	)
	return order, nil
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

func validate(in CreateOrderInput) error {
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
