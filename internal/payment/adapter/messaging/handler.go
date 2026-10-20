// Package messaging provides the RabbitMQ delivery adapter for payment use cases.
package messaging

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/elita001/order-microservices/internal/payment/application"
	"github.com/elita001/order-microservices/pkg/events"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
)

type Service interface {
	ProcessPayment(ctx context.Context, in application.ProcessPaymentInput) error
}

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// RoutingKeys lists the events the payment service subscribes to.
func (h *Handler) RoutingKeys() []string {
	return []string{events.OrderCreatedType}
}

func (h *Handler) Handle(ctx context.Context, d rabbitmq.Delivery) error {
	if d.RoutingKey != events.OrderCreatedType {
		logger.FromContext(ctx).Warn("unexpected event ignored")
		return nil
	}

	var event events.OrderCreated
	if err := json.Unmarshal(d.Body, &event); err != nil {
		// Redelivering a malformed message cannot help.
		return rabbitmq.Permanent(fmt.Errorf("decode %s: %w", d.RoutingKey, err))
	}

	err := h.service.ProcessPayment(ctx, application.ProcessPaymentInput{
		OrderID:    event.OrderID,
		CustomerID: event.CustomerID,
		Amount:     event.TotalAmount,
		Currency:   event.Currency,
	})
	if application.IsValidationError(err) {
		return rabbitmq.Permanent(err)
	}
	return err
}
