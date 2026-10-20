// Package messaging provides the RabbitMQ delivery adapter for delivery use cases.
package messaging

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/elita001/order-microservices/internal/delivery/application"
	"github.com/elita001/order-microservices/pkg/events"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
)

type Service interface {
	ShipOrder(ctx context.Context, in application.ShipOrderInput) error
}

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// RoutingKeys lists the events the delivery service subscribes to.
func (h *Handler) RoutingKeys() []string {
	return []string{events.PaymentSucceededType}
}

func (h *Handler) Handle(ctx context.Context, d rabbitmq.Delivery) error {
	if d.RoutingKey != events.PaymentSucceededType {
		logger.FromContext(ctx).Warn("unexpected event ignored")
		return nil
	}

	var event events.PaymentSucceeded
	if err := json.Unmarshal(d.Body, &event); err != nil {
		// Redelivering a malformed message cannot help.
		return rabbitmq.Permanent(fmt.Errorf("decode %s: %w", d.RoutingKey, err))
	}

	err := h.service.ShipOrder(ctx, application.ShipOrderInput{OrderID: event.OrderID, CustomerID: event.CustomerID})
	if application.IsValidationError(err) {
		return rabbitmq.Permanent(err)
	}
	return err
}
