// Package gateway is the entry point for external clients: it routes requests to the
// services and applies the edge concerns: request IDs, rate limits, idempotency, timeouts.
package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/elita001/order-microservices/internal/gateway/idempotency"
	"github.com/elita001/order-microservices/internal/gateway/proxy"
	"github.com/elita001/order-microservices/internal/gateway/ratelimit"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/metrics"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

type Config struct {
	OrderServiceURL *url.URL
	Upstream        proxy.Config
	ReadLimit       ratelimit.Policy // GET requests
	WriteLimit      ratelimit.Policy // POST requests
	Idempotency     idempotency.Config
	RedisTimeout    time.Duration // per Redis call; Redis failures are tolerated (fail-open)
}

func NewRouter(cfg Config, rdb *redis.Client, log *slog.Logger) http.Handler {
	limiter := ratelimit.New(rdb, cfg.RedisTimeout)
	idempotent := idempotency.New(rdb, cfg.Idempotency, cfg.RedisTimeout)
	orders := proxy.New(cfg.OrderServiceURL, cfg.Upstream)

	r := chi.NewRouter()
	// metrics.HTTP sits outside Recoverer, so a recovered panic is counted as a 500.
	r.Use(httpx.RequestID, httpx.Logger(log), metrics.HTTP, telemetry.RouteName, httpx.Recoverer)

	// The gateway is ready while it runs: Redis is optional (fail-open), and an unavailable
	// upstream must not take the whole entry point out of the load balancer.
	httpx.RegisterHealth(r, func(context.Context) error { return nil })

	r.Route("/api/v1/orders", func(r chi.Router) {
		r.With(limiter.Middleware(cfg.WriteLimit), idempotent.Middleware).Post("/", orders.ServeHTTP)
		r.With(limiter.Middleware(cfg.ReadLimit)).Get("/", orders.ServeHTTP)
		r.With(limiter.Middleware(cfg.ReadLimit)).Get("/{id}", orders.ServeHTTP)
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})
	return r
}
