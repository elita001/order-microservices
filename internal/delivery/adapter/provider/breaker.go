package provider

import (
	"context"
	"log/slog"

	"github.com/elita001/order-microservices/internal/delivery/application"
	"github.com/elita001/order-microservices/pkg/breaker"
)

// WithBreaker protects the carrier with a circuit breaker: while its API is down,
// shipments fail fast with breaker.ErrOpen and are retried later with backoff.
func WithBreaker(inner application.Carrier, cfg breaker.Config, log *slog.Logger) application.Carrier {
	return &breakerCarrier{inner: inner, b: breaker.New[application.RegisterShipmentResult]("carrier", cfg, nil, log)}
}

type breakerCarrier struct {
	inner application.Carrier
	b     *breaker.Breaker[application.RegisterShipmentResult]
}

func (c *breakerCarrier) RegisterShipment(ctx context.Context, req application.RegisterShipmentRequest) (application.RegisterShipmentResult, error) {
	return c.b.Execute(func() (application.RegisterShipmentResult, error) {
		return c.inner.RegisterShipment(ctx, req)
	})
}
