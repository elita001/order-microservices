// Package provider contains adapters for external payment providers.
package provider

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/elita001/order-microservices/internal/payment/application"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

var tracer = telemetry.Tracer("github.com/elita001/order-microservices/internal/payment/adapter/provider")

// StubConfig is usually loaded from environment variables with the PAYMENT_STUB_ prefix.
type StubConfig struct {
	FailureRate float64       `env:"FAILURE_RATE" envDefault:"0.2"` // share of declined payments, 0..1
	ErrorRate   float64       `env:"ERROR_RATE" envDefault:"0"`     // share of calls failing with a technical error, 0..1
	MinDelay    time.Duration `env:"MIN_DELAY" envDefault:"100ms"`
	MaxDelay    time.Duration `env:"MAX_DELAY" envDefault:"1s"`
}

var declineReasons = []string{"insufficient_funds", "card_declined", "card_expired"}

// ErrUnavailable is a simulated technical failure of the provider's API.
var ErrUnavailable = errors.New("payment provider API is temporarily unavailable")

// Stub simulates an external payment provider: it responds with a random delay, declines
// a share of payments (a business outcome), and fails a share of calls with a technical
// error (the caller retries; set ERROR_RATE=1 to simulate an outage).
//
// Like a real provider, it is idempotent: the outcome is derived from the idempotency
// key, so a retry of the same payment always gets the same answer.
type Stub struct {
	cfg StubConfig
}

var _ application.Provider = (*Stub)(nil)

func NewStub(cfg StubConfig) *Stub {
	return &Stub{cfg: cfg}
}

func (s *Stub) Charge(ctx context.Context, req application.ChargeRequest) (result application.ChargeResult, err error) {
	// A client span for the external call shows on the trace how long the provider took.
	ctx, span := tracer.Start(ctx, "payment_provider.charge", trace.WithSpanKind(trace.SpanKindClient))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "charge failed")
		} else {
			span.SetAttributes(attribute.Bool("payment.approved", result.Approved))
		}
		span.End()
	}()

	select {
	case <-ctx.Done():
		return application.ChargeResult{}, fmt.Errorf("payment provider: %w", ctx.Err())
	case <-time.After(s.delay()):
	}
	// Technical errors are random, not derived from the key: a retry may succeed.
	if rand.Float64() < s.cfg.ErrorRate { //nolint:gosec // simulated failures, not security
		return application.ChargeResult{}, ErrUnavailable
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(req.IdempotencyKey))
	sum := h.Sum64()
	result = application.ChargeResult{TransactionID: fmt.Sprintf("stub_%016x", sum)}

	if float64(sum%10_000)/10_000 < s.cfg.FailureRate {
		result.DeclineReason = declineReasons[(sum/10_000)%uint64(len(declineReasons))]
		return result, nil
	}
	result.Approved = true
	return result, nil
}

func (s *Stub) delay() time.Duration {
	spread := s.cfg.MaxDelay - s.cfg.MinDelay
	if spread <= 0 {
		return max(s.cfg.MinDelay, 0)
	}
	return s.cfg.MinDelay + rand.N(spread) //nolint:gosec // simulated latency, not security
}
