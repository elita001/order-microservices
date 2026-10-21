// Package breaker provides circuit breakers for calls to dependencies.
//
// After Failures consecutive failures the breaker opens, and calls fail immediately
// with ErrOpen instead of waiting for timeouts of a dependency that is down. After
// OpenTimeout it lets a few trial calls through (half-open): if they succeed, it closes.
package breaker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sony/gobreaker/v2"
)

// ErrOpen is returned without calling the dependency while the breaker is open.
var ErrOpen = errors.New("circuit breaker is open")

// Config is usually loaded from environment variables with a <DEPENDENCY>_BREAKER_ prefix.
type Config struct {
	Failures         uint32        `env:"FAILURES" envDefault:"5"`           // consecutive failures that open the breaker
	OpenTimeout      time.Duration `env:"OPEN_TIMEOUT" envDefault:"30s"`     // how long it stays open
	HalfOpenRequests uint32        `env:"HALF_OPEN_REQUESTS" envDefault:"1"` // trial calls after OpenTimeout
}

type Breaker[T any] struct {
	cb *gobreaker.CircuitBreaker[T]
}

// New creates a breaker. isFailure decides which errors mean the dependency is unhealthy;
// nil counts every error. Canceled calls (the caller gave up) are never counted.
func New[T any](name string, cfg Config, isFailure func(error) bool, log *slog.Logger) *Breaker[T] {
	log = log.With("component", "circuit_breaker", "breaker", name)
	return &Breaker[T]{cb: gobreaker.NewCircuitBreaker[T](gobreaker.Settings{
		Name:        name,
		MaxRequests: cfg.HalfOpenRequests,
		Timeout:     cfg.OpenTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= cfg.Failures
		},
		IsSuccessful: func(err error) bool {
			return err == nil || (isFailure != nil && !isFailure(err))
		},
		IsExcluded: func(err error) bool {
			return errors.Is(err, context.Canceled)
		},
		// State changes are logged once instead of an error on every rejected call.
		OnStateChange: func(_ string, from, to gobreaker.State) {
			if to == gobreaker.StateOpen {
				log.Error("circuit breaker opened: dependency calls are suspended", "from", from.String(), "open_for", cfg.OpenTimeout.String())
				return
			}
			log.Warn("circuit breaker state changed", "from", from.String(), "to", to.String())
		},
	})}
}

// Execute calls fn unless the breaker is open.
func (b *Breaker[T]) Execute(fn func() (T, error)) (T, error) {
	result, err := b.cb.Execute(fn)
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return result, ErrOpen
	}
	return result, err
}
