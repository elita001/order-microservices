// Package provider contains adapters for external payment providers.
package provider

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"time"

	"github.com/elita001/order-microservices/internal/payment/application"
)

// StubConfig is usually loaded from environment variables with the PAYMENT_STUB_ prefix.
type StubConfig struct {
	FailureRate float64       `env:"FAILURE_RATE" envDefault:"0.2"` // share of declined payments, 0..1
	MinDelay    time.Duration `env:"MIN_DELAY" envDefault:"100ms"`
	MaxDelay    time.Duration `env:"MAX_DELAY" envDefault:"1s"`
}

var declineReasons = []string{"insufficient_funds", "card_declined", "card_expired"}

// Stub simulates an external payment provider: it responds with a random delay and
// declines a share of payments.
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

func (s *Stub) Charge(ctx context.Context, req application.ChargeRequest) (application.ChargeResult, error) {
	select {
	case <-ctx.Done():
		return application.ChargeResult{}, fmt.Errorf("payment provider: %w", ctx.Err())
	case <-time.After(s.delay()):
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(req.IdempotencyKey))
	sum := h.Sum64()
	result := application.ChargeResult{TransactionID: fmt.Sprintf("stub_%016x", sum)}

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
	return s.cfg.MinDelay + rand.N(spread)
}
