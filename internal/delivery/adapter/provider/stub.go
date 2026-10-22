// Package provider contains adapters for external delivery providers (carriers).
package provider

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/elita001/order-microservices/internal/delivery/application"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

var tracer = telemetry.Tracer("github.com/elita001/order-microservices/internal/delivery/adapter/provider")

// StubConfig is usually loaded from environment variables with the DELIVERY_STUB_ prefix.
type StubConfig struct {
	ErrorRate float64       `env:"ERROR_RATE" envDefault:"0.1"` // share of calls failing with a technical error, 0..1
	MinDelay  time.Duration `env:"MIN_DELAY" envDefault:"100ms"`
	MaxDelay  time.Duration `env:"MAX_DELAY" envDefault:"500ms"`
}

var carriers = []string{"CDEK", "Boxberry", "Russian Post"}

// ErrUnavailable is a simulated technical failure of the carrier's API.
var ErrUnavailable = errors.New("carrier API is temporarily unavailable")

// Stub simulates an external carrier API: it responds with a random delay and fails
// randomly with a technical error, so the consumer has to retry.
//
// Like a real carrier, it is idempotent: the carrier, tracking number, and estimated
// delivery date are derived from the idempotency key, so a retry gets the same parcel.
type Stub struct {
	cfg StubConfig
	now func() time.Time
}

var _ application.Carrier = (*Stub)(nil)

func NewStub(cfg StubConfig) *Stub {
	return &Stub{cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Stub) RegisterShipment(ctx context.Context, req application.RegisterShipmentRequest) (_ application.RegisterShipmentResult, err error) {
	// A client span for the external call shows on the trace how long the carrier took.
	ctx, span := tracer.Start(ctx, "carrier.register_shipment", trace.WithSpanKind(trace.SpanKindClient))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "register shipment failed")
		}
		span.End()
	}()

	select {
	case <-ctx.Done():
		return application.RegisterShipmentResult{}, fmt.Errorf("carrier: %w", ctx.Err())
	case <-time.After(s.delay()):
	}
	// Technical errors are random, not derived from the key: a retry may succeed.
	if rand.Float64() < s.cfg.ErrorRate {
		return application.RegisterShipmentResult{}, ErrUnavailable
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(req.IdempotencyKey))
	sum := h.Sum64()
	days := 2 + int(sum%6) // 2..7 days

	return application.RegisterShipmentResult{
		Carrier:             carriers[sum%uint64(len(carriers))],
		TrackingNumber:      "OF" + strings.ToUpper(fmt.Sprintf("%012x", sum&0xffffffffffff)),
		EstimatedDeliveryAt: s.now().Truncate(24*time.Hour).AddDate(0, 0, days),
	}, nil
}

func (s *Stub) delay() time.Duration {
	spread := s.cfg.MaxDelay - s.cfg.MinDelay
	if spread <= 0 {
		return max(s.cfg.MinDelay, 0)
	}
	return s.cfg.MinDelay + rand.N(spread)
}
