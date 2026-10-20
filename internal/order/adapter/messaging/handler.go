// Package messaging provides the RabbitMQ delivery adapter for order use cases.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/pkg/events"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
)

type Service interface {
	CompletePayment(ctx context.Context, in application.CompletePaymentInput) error
	MarkShipped(ctx context.Context, orderID uuid.UUID) error
}

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// RoutingKeys lists the events the order service subscribes to.
func (h *Handler) RoutingKeys() []string {
	return []string{events.PaymentSucceededType, events.PaymentFailedType, events.DeliveryCreatedType}
}

func (h *Handler) Handle(ctx context.Context, d rabbitmq.Delivery) error {
	var err error
	switch d.RoutingKey {
	case events.PaymentSucceededType:
		var event events.PaymentSucceeded
		if err := decode(d, &event); err != nil {
			return err
		}
		err = h.service.CompletePayment(ctx, application.CompletePaymentInput{OrderID: event.OrderID, Succeeded: true})
	case events.PaymentFailedType:
		var event events.PaymentFailed
		if err := decode(d, &event); err != nil {
			return err
		}
		err = h.service.CompletePayment(ctx, application.CompletePaymentInput{OrderID: event.OrderID, Reason: event.Reason})
	case events.DeliveryCreatedType:
		var event events.DeliveryCreated
		if err := decode(d, &event); err != nil {
			return err
		}
		err = h.service.MarkShipped(ctx, event.OrderID)
	default:
		logger.FromContext(ctx).Warn("unexpected event ignored")
		return nil
	}

	var validationErr *domain.ValidationError
	if errors.As(err, &validationErr) {
		return rabbitmq.Permanent(err)
	}
	return err
}

// decode rejects malformed messages permanently: redelivering them cannot help.
func decode(d rabbitmq.Delivery, dst any) error {
	if err := json.Unmarshal(d.Body, dst); err != nil {
		return rabbitmq.Permanent(fmt.Errorf("decode %s: %w", d.RoutingKey, err))
	}
	return nil
}
