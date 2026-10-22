// Package application implements notification use cases and defines their ports.
package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/notification/domain"
)

// Repository is the persistence port required by the notification use cases.
type Repository interface {
	// SendOnce records the notification and calls send in one transaction. If a
	// notification of the same kind was already sent for the order, it returns false
	// without calling send. If send fails, nothing is recorded, so a retry sends again.
	SendOnce(ctx context.Context, n *domain.Notification, send func(ctx context.Context) error) (bool, error)
	// Sent reports whether a notification of this kind has been sent for the order.
	Sent(ctx context.Context, kind domain.Kind, orderID uuid.UUID) (bool, error)
}

// CustomerDirectory is the port of the service that knows customers' contact details.
type CustomerDirectory interface {
	Email(ctx context.Context, customerID uuid.UUID) (string, error)
}

// Mailer is the port of the email delivery provider.
type Mailer interface {
	Send(ctx context.Context, email Email) error
}

type Email struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// ErrInvalidRecipient means the email address is rejected; retrying cannot help.
var ErrInvalidRecipient = errors.New("invalid recipient")

// ErrOutOfOrder means an event arrived before the one it follows; it is retried later.
var ErrOutOfOrder = errors.New("event arrived before the event it follows")

// Logger is the logging capability required by the application service.
type Logger interface {
	Info(message string, args ...any)
}

// Metrics records business metrics; a Prometheus adapter implements it.
type Metrics interface {
	NotificationSent(kind domain.Kind)
}

type Service struct {
	repo      Repository
	customers CustomerDirectory
	mailer    Mailer
	logger    Logger
	metrics   Metrics
	now       func() time.Time
}

func NewService(repo Repository, customers CustomerDirectory, mailer Mailer, logger Logger) *Service {
	return &Service{
		repo:      repo,
		customers: customers,
		mailer:    mailer,
		logger:    logger,
		metrics:   noopMetrics{},
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// WithMetrics enables business metrics; without it they are not recorded.
func (s *Service) WithMetrics(m Metrics) *Service {
	s.metrics = m
	return s
}

type noopMetrics struct{}

func (noopMetrics) NotificationSent(domain.Kind) {}

type Item struct {
	ProductID string
	Quantity  int
	Price     int64
}

type OrderCreatedInput struct {
	OrderID     uuid.UUID
	CustomerID  uuid.UUID
	Items       []Item
	TotalAmount int64
	Currency    string
}

type PaymentSucceededInput struct {
	OrderID       uuid.UUID
	CustomerID    uuid.UUID
	Amount        int64
	Currency      string
	TransactionID string
}

type PaymentFailedInput struct {
	OrderID    uuid.UUID
	CustomerID uuid.UUID
	Amount     int64
	Currency   string
	Reason     string
}

type OrderShippedInput struct {
	OrderID             uuid.UUID
	CustomerID          uuid.UUID
	Carrier             string
	TrackingNumber      string
	EstimatedDeliveryAt time.Time
}

func (s *Service) NotifyOrderCreated(ctx context.Context, in OrderCreatedInput) error {
	return s.notify(ctx, domain.KindOrderCreated, in.OrderID, in.CustomerID, orderCreatedContent(in))
}

func (s *Service) NotifyPaymentSucceeded(ctx context.Context, in PaymentSucceededInput) error {
	return s.notify(ctx, domain.KindPaymentSucceeded, in.OrderID, in.CustomerID, paymentSucceededContent(in))
}

func (s *Service) NotifyPaymentFailed(ctx context.Context, in PaymentFailedInput) error {
	return s.notify(ctx, domain.KindPaymentFailed, in.OrderID, in.CustomerID, paymentFailedContent(in))
}

// NotifyOrderShipped sends the "shipped" email only after the "payment received" one.
// Events from different services can arrive in any order; a shipment is created only
// after a successful payment, so a missing payment email means its event is still on
// the way, and this one is retried with backoff until it arrives.
func (s *Service) NotifyOrderShipped(ctx context.Context, in OrderShippedInput) error {
	if in.OrderID != uuid.Nil {
		paymentSent, err := s.repo.Sent(ctx, domain.KindPaymentSucceeded, in.OrderID)
		if err != nil {
			return fmt.Errorf("check payment notification for order %s: %w", in.OrderID, err)
		}
		if !paymentSent {
			return fmt.Errorf("%w: the payment email for order %s has not been sent yet", ErrOutOfOrder, in.OrderID)
		}
	}
	return s.notify(ctx, domain.KindOrderShipped, in.OrderID, in.CustomerID, orderShippedContent(in))
}

// notify sends one email of the given kind per order. Delivery is at-least-once: if the
// process crashes after the email is sent but before the record is committed, a retry
// sends it again. That is the best SMTP allows, and a rare duplicate email is acceptable.
func (s *Service) notify(ctx context.Context, kind domain.Kind, orderID, customerID uuid.UUID, c content) error {
	if orderID == uuid.Nil {
		return &domain.ValidationError{Field: "order_id", Message: "is required"}
	}
	if customerID == uuid.Nil {
		return &domain.ValidationError{Field: "customer_id", Message: "is required"}
	}

	recipient, err := s.customers.Email(ctx, customerID)
	if err != nil {
		return fmt.Errorf("get email of customer %s: %w", customerID, err)
	}
	html, err := c.html()
	if err != nil {
		return err
	}

	n := &domain.Notification{
		ID:         uuid.New(),
		Kind:       kind,
		OrderID:    orderID,
		CustomerID: customerID,
		Recipient:  recipient,
		Subject:    c.Subject,
		SentAt:     s.now(),
	}
	sent, err := s.repo.SendOnce(ctx, n, func(ctx context.Context) error {
		return s.mailer.Send(ctx, Email{To: recipient, Subject: c.Subject, Text: c.text(), HTML: html})
	})
	if err != nil {
		return fmt.Errorf("send %s notification for order %s: %w", kind, orderID, err)
	}
	if !sent {
		s.logger.Info("notification already sent, skipping", "kind", kind, "order_id", orderID)
		return nil
	}

	s.metrics.NotificationSent(kind)
	// The email address is personal data, so only the customer ID goes to the logs.
	s.logger.Info("notification sent", "kind", kind, "order_id", orderID, "customer_id", customerID)
	return nil
}

// IsPermanentError reports whether err is caused by input that retrying cannot fix.
func IsPermanentError(err error) bool {
	var validationErr *domain.ValidationError
	return errors.As(err, &validationErr) || errors.Is(err, ErrInvalidRecipient)
}
