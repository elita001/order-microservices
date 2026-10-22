// Package ratelimit limits the request rate per client using Redis.
package ratelimit

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"github.com/elita001/order-microservices/pkg/breaker"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
)

// Policy allows Rate requests per second with bursts of up to Burst requests.
type Policy struct {
	Name  string // separates the counters of different policies
	Rate  int
	Burst int
}

// Limiter uses GCRA (a token-bucket variant) in a Lua script, so the limit is exact
// even with many gateway replicas sharing one Redis.
type Limiter struct {
	limiter *redis_rate.Limiter
	timeout time.Duration // per Redis call
}

func New(rdb *redis.Client, timeout time.Duration) *Limiter {
	return &Limiter{limiter: redis_rate.NewLimiter(rdb), timeout: timeout}
}

// Middleware limits requests per client IP. It fails open: if Redis is unavailable,
// requests pass, because availability matters more than an exact limit here.
func (l *Limiter) Middleware(policy Policy) func(http.Handler) http.Handler {
	limit := redis_rate.Limit{Rate: policy.Rate, Burst: policy.Burst, Period: time.Second}
	limitHeader := strconv.Itoa(policy.Rate)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := "ratelimit:" + policy.Name + ":" + clientIP(r)
			ctx, cancel := context.WithTimeout(r.Context(), l.timeout)
			res, err := l.limiter.Allow(ctx, key, limit)
			cancel()
			if err != nil {
				decisions.WithLabelValues(policy.Name, "bypassed").Inc()
				if !errors.Is(err, breaker.ErrOpen) { // an open breaker is logged once by itself
					logger.FromContext(r.Context()).Warn("rate limiter unavailable, request allowed", "error", err)
				}
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("X-RateLimit-Limit", limitHeader)
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
			if res.Allowed == 0 {
				decisions.WithLabelValues(policy.Name, "rejected").Inc()
				retryAfter := int(math.Ceil(res.RetryAfter.Seconds()))
				w.Header().Set("Retry-After", strconv.Itoa(max(retryAfter, 1)))
				httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, retry later")
				return
			}
			decisions.WithLabelValues(policy.Name, "allowed").Inc()
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP uses the TCP peer address: the gateway is the edge of the system. Behind a
// load balancer, it must instead take X-Forwarded-For set by that trusted balancer.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
