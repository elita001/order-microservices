package httpx

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/elita001/order-microservices/pkg/logger"
)

// Logger logs each request and stores a logger with request_id in the context.
// It must be placed after middleware.RequestID.
func Logger(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			l := base.With("request_id", middleware.GetReqID(r.Context()))
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r.WithContext(logger.WithContext(r.Context(), l)))

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			l.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// Recoverer catches panics, logs the stack trace, and responds with 500.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // required by net/http
				panic(rec)
			}
			logger.FromContext(r.Context()).Error("panic recovered",
				"panic", rec, "stack", string(debug.Stack()))
			WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
