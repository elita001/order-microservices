package httpx

import (
	"context"
	"net/http"
	"time"
)

// Timeout gives every request a deadline. Handlers and the database calls they make stop
// at the deadline, so a service answers (e.g., 503 when its database hangs) before the
// caller gives up: the deadline must be shorter than the gateway's upstream timeout.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
