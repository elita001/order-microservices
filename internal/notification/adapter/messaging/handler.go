// Package messaging provides the RabbitMQ delivery adapter for notification use cases.
package messaging

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/elita001/order-microservices/internal/notification/application"
	"github.com/elita001/order-microservices/pkg/events"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
)

type Service interface {
	NotifyOrderCreated(ctx context.Context, in application.OrderCreatedInput) error
	NotifyPaymentSucceeded(ctx context.Context, in application.PaymentSucceededInput) error
	NotifyPaymentFailed(ctx context.Context, in application.PaymentFailedInput) error
	NotifyOrderShipped(ctx context.Context, in application.OrderShippedInput) error
}

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// RoutingKeys lists the events the notification service subscribes to.
func (h *Handler) RoutingKeys() []string {
	return []string{
		events.OrderCreatedType,
		events.PaymentSucceededType,
		events.PaymentFailedType,
		events.DeliveryCreatedType,
	}
}

func (h *Handler) Handle(ctx context.Context, d rabbitmq.Delivery) error {
	err := h.dispatch(ctx, d)
	if application.IsPermanentError(err) {
		return rabbitmq.Permanent(err)
	}
	return err
}

func (h *Handler) dispatch(ctx context.Context, d rabbitmq.Delivery) error {
	switch d.RoutingKey {
	case events.OrderCreatedType:
		var e events.OrderCreated
		if err := decode(d, &e); err != nil {
			return err
		}
		items := make([]application.Item, 0, len(e.Items))
		for _, item := range e.Items {
			items = append(items, application.Item{ProductID: item.ProductID, Quantity: item.Quantity, Price: item.Price})
		}
		return h.service.NotifyOrderCreated(ctx, application.OrderCreatedInput{
			OrderID: e.OrderID, CustomerID: e.CustomerID, Items: items, TotalAmount: e.TotalAmount, Currency: e.Currency,
		})
	case events.PaymentSucceededType:
		var e events.PaymentSucceeded
		if err := decode(d, &e); err != nil {
			return err
		}
		return h.service.NotifyPaymentSucceeded(ctx, application.PaymentSucceededInput{
			OrderID: e.OrderID, CustomerID: e.CustomerID, Amount: e.Amount, Currency: e.Currency, TransactionID: e.TransactionID,
		})
	case events.PaymentFailedType:
		var e events.PaymentFailed
		if err := decode(d, &e); err != nil {
			return err
		}
		return h.service.NotifyPaymentFailed(ctx, application.PaymentFailedInput{
			OrderID: e.OrderID, CustomerID: e.CustomerID, Amount: e.Amount, Currency: e.Currency, Reason: e.Reason,
		})
	case events.DeliveryCreatedType:
		var e events.DeliveryCreated
		if err := decode(d, &e); err != nil {
			return err
		}
		return h.service.NotifyOrderShipped(ctx, application.OrderShippedInput{
			OrderID: e.OrderID, CustomerID: e.CustomerID, Carrier: e.Carrier,
			TrackingNumber: e.TrackingNumber, EstimatedDeliveryAt: e.EstimatedDeliveryAt,
		})
	default:
		logger.FromContext(ctx).Warn("unexpected event ignored")
		return nil
	}
}

// decode rejects malformed messages permanently: redelivering them cannot help.
func decode(d rabbitmq.Delivery, dst any) error {
	if err := json.Unmarshal(d.Body, dst); err != nil {
		return rabbitmq.Permanent(fmt.Errorf("decode %s: %w", d.RoutingKey, err))
	}
	return nil
}
