// Package gateway is the entry point for external clients: it serves the public REST API,
// calls the services over gRPC, and applies the edge concerns: authentication, request IDs,
// rate limits, idempotency, and deadlines.
package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	orderv1 "github.com/elita001/order-microservices/gen/go/orderflow/order/v1"
	"github.com/elita001/order-microservices/internal/gateway/idempotency"
	"github.com/elita001/order-microservices/internal/gateway/ratelimit"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/metrics"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

type Config struct {
	// Clients of the services. Each call carries the user's access token, the request ID,
	// and the trace context, and has a deadline.
	Orders    orderv1.OrderServiceClient
	Customers customerv1.CustomerServiceClient

	ReadLimit    ratelimit.Policy // GET requests
	WriteLimit   ratelimit.Policy // POST requests
	Idempotency  idempotency.Config
	RedisTimeout time.Duration // per Redis call; Redis failures are tolerated (fail-open)
	// Verifier checks access tokens. The gateway rejects unauthenticated requests early;
	// the services verify the token again and never trust identity from headers.
	Verifier *auth.Verifier
}

var authFailures = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "gateway_auth_failures_total",
	Help: "Requests rejected with 401, by reason: missing_token or invalid_token.",
}, []string{"reason"})

func NewRouter(cfg Config, rdb *redis.Client, log *slog.Logger) http.Handler {
	limiter := ratelimit.New(rdb, cfg.RedisTimeout)
	idempotent := idempotency.New(rdb, cfg.Idempotency, cfg.RedisTimeout)
	orders := orders{client: cfg.Orders}
	customers := customers{client: cfg.Customers}
	// Authentication comes first: rate limits and idempotency keys are per user.
	authenticate := auth.Middleware(cfg.Verifier, func(reason string) { authFailures.WithLabelValues(reason).Inc() })
	read := limiter.Middleware(cfg.ReadLimit)

	r := chi.NewRouter()
	// metrics.HTTP sits outside Recoverer, so a recovered panic is counted as a 500.
	r.Use(httpx.RequestID, httpx.Logger(log), metrics.HTTP, telemetry.RouteName, httpx.Recoverer)

	// The gateway is ready while it runs: Redis is optional (fail-open), and an unavailable
	// service must not take the whole entry point out of the load balancer.
	httpx.RegisterHealth(r, func(context.Context) error { return nil })

	r.Route("/api/v1/orders", func(r chi.Router) {
		r.Use(authenticate)
		r.With(limiter.Middleware(cfg.WriteLimit), idempotent.Middleware).Post("/", orders.create)
		r.With(read).Get("/", orders.list)
		r.With(read).Get("/{id}", orders.get)
	})
	r.Route("/api/v1/customers", func(r chi.Router) {
		r.Use(authenticate, read)
		r.Get("/me", customers.me)
		r.Get("/{id}", customers.get)
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})
	return r
}
