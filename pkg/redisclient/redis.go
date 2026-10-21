// Package redisclient creates a Redis client with short timeouts.
//
// Redis is an accelerator here, not a source of truth: callers fail open when it is
// unavailable. Short timeouts matter more than retries, because a slow Redis would
// otherwise slow down every request.
package redisclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elita001/order-microservices/pkg/breaker"
)

// Config is usually loaded from environment variables with the REDIS_ prefix.
type Config struct {
	URL          string        `env:"URL,required"` // redis://:password@host:6379/0
	DialTimeout  time.Duration `env:"DIAL_TIMEOUT" envDefault:"500ms"`
	ReadTimeout  time.Duration `env:"READ_TIMEOUT" envDefault:"200ms"`
	WriteTimeout time.Duration `env:"WRITE_TIMEOUT" envDefault:"200ms"`
	// OperationTimeout bounds one call including dialing, DNS, and the client's internal
	// retries. Callers apply it via context: a hanging Redis (e.g., its DNS name does not
	// resolve) is worse than a failed one, because it delays every request.
	OperationTimeout time.Duration `env:"OPERATION_TIMEOUT" envDefault:"300ms"`

	// Breaker stops calling Redis after consecutive failures, so requests do not wait
	// OperationTimeout each while Redis is down; callers get breaker.ErrOpen at once.
	Breaker breaker.Config `envPrefix:"BREAKER_"`
}

// New does not connect: the client connects lazily and reconnects on its own.
// Every command goes through a circuit breaker.
func New(cfg Config, log *slog.Logger) (*redis.Client, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opts.DialTimeout = cfg.DialTimeout
	opts.ReadTimeout = cfg.ReadTimeout
	opts.WriteTimeout = cfg.WriteTimeout
	opts.PoolTimeout = cfg.ReadTimeout + cfg.WriteTimeout
	opts.MaxRetries = 1
	opts.DialerRetries = 1
	// Without this, go-redis ignores context deadlines while waiting for a reply,
	// and OperationTimeout would have no effect.
	opts.ContextTimeoutEnabled = true

	client := redis.NewClient(opts)
	client.AddHook(&breakerHook{b: breaker.New[struct{}]("redis", cfg.Breaker, isFailure, log)})
	return client, nil
}

// IsUnavailable reports whether err means Redis could not be used: a network error,
// a timeout, or an open circuit breaker. Callers fall back to their Redis-less path.
func IsUnavailable(err error) bool {
	return err != nil && isFailure(err)
}

// isFailure counts only infrastructure errors. A reply from Redis, even an error reply
// (redis.Nil, NOSCRIPT, WRONGTYPE), proves that Redis is up.
func isFailure(err error) bool {
	var replyErr redis.Error
	return !errors.As(err, &replyErr)
}

// breakerHook wraps every command and pipeline in the circuit breaker.
type breakerHook struct {
	b *breaker.Breaker[struct{}]
}

func (h *breakerHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (h *breakerHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		_, err := h.b.Execute(func() (struct{}, error) { return struct{}{}, next(ctx, cmd) })
		if errors.Is(err, breaker.ErrOpen) {
			cmd.SetErr(err)
		}
		return err
	}
}

func (h *breakerHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		_, err := h.b.Execute(func() (struct{}, error) { return struct{}{}, next(ctx, cmds) })
		if errors.Is(err, breaker.ErrOpen) {
			for _, cmd := range cmds {
				cmd.SetErr(err)
			}
		}
		return err
	}
}
