// Package redisclient creates a Redis client with short timeouts.
//
// Redis is an accelerator here, not a source of truth: callers fail open when it is
// unavailable. Short timeouts matter more than retries, because a slow Redis would
// otherwise slow down every request.
package redisclient

import (
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
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
}

// New does not connect: the client connects lazily and reconnects on its own.
func New(cfg Config) (*redis.Client, error) {
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
	return redis.NewClient(opts), nil
}
