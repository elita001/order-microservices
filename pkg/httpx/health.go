package httpx

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// RegisterHealth adds the liveness (/healthz) and readiness (/readyz) probes.
// ready reports whether the dependencies required to serve traffic are available.
func RegisterHealth(r chi.Router, ready func(ctx context.Context) error) {
	// liveness: the process is alive
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// readiness: the service is ready to accept traffic
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			WriteError(w, http.StatusServiceUnavailable, "not_ready", "dependencies unavailable")
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
