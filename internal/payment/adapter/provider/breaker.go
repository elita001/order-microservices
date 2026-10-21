package provider

import (
	"context"
	"log/slog"

	"github.com/elita001/order-microservices/internal/payment/application"
	"github.com/elita001/order-microservices/pkg/breaker"
)

// WithBreaker protects the provider with a circuit breaker: while the provider is down,
// payments fail fast with breaker.ErrOpen and are retried later with backoff instead of
// every message waiting for a timeout. Declines are successful calls and do not count.
func WithBreaker(inner application.Provider, cfg breaker.Config, log *slog.Logger) application.Provider {
	return &breakerProvider{inner: inner, b: breaker.New[application.ChargeResult]("payment_provider", cfg, nil, log)}
}

type breakerProvider struct {
	inner application.Provider
	b     *breaker.Breaker[application.ChargeResult]
}

func (p *breakerProvider) Charge(ctx context.Context, req application.ChargeRequest) (application.ChargeResult, error) {
	return p.b.Execute(func() (application.ChargeResult, error) {
		return p.inner.Charge(ctx, req)
	})
}
