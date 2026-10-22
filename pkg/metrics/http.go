// Package metrics exposes Prometheus metrics: the /metrics handler and RED metrics
// (rate, errors, duration) of HTTP servers. Other packages register their own metrics
// in the default registry, so one handler serves everything a process exports.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "HTTP requests by method, route pattern, and status code.",
	}, []string{"method", "route", "status"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration by method and route pattern.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "route"})

	httpInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "http_requests_in_flight",
		Help: "HTTP requests being served.",
	})
)

// Probes and scrapes are frequent and uninteresting; they would only add noise.
var skippedRoutes = map[string]bool{"/metrics": true, "/healthz": true, "/readyz": true}

// Handler serves the metrics for Prometheus.
func Handler() http.Handler {
	return promhttp.Handler()
}

// HTTP records RED metrics. It must be used on a chi router: the route label is the
// matched pattern ("/api/v1/orders/{id}"), not the path. With raw paths every order ID
// would create new time series, and Prometheus memory would grow without bound.
func HTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		httpInFlight.Inc()
		defer httpInFlight.Dec()

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		route := "unmatched"
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
			route = rctx.RoutePattern()
		}
		if skippedRoutes[route] {
			return
		}
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		method := normalizeMethod(r.Method)
		httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
		httpDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
	})
}

// normalizeMethod keeps the label set bounded: clients can send any method string.
func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	}
	return "OTHER"
}
