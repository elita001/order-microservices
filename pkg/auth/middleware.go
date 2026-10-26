package auth

import (
	"errors"
	"net/http"

	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
)

// Middleware rejects requests without a valid access token with 401 and stores the
// principal in the context. onFailure, if set, is called with the reason (for metrics).
func Middleware(v *Verifier, onFailure func(reason string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := BearerToken(r)
			if err == nil {
				ctx := r.Context()
				if ctx, err = v.Authenticate(ctx, raw); err == nil {
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			reason := FailureReason(err)
			if onFailure != nil {
				onFailure(reason)
			}
			if errors.Is(err, ErrKeysUnavailable) {
				logger.FromContext(r.Context()).Error("cannot verify tokens: signing keys are unavailable", "error", err)
				w.Header().Set("Retry-After", "5")
				httpx.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "authentication is temporarily unavailable, retry later")
				return
			}
			logger.FromContext(r.Context()).Info("request rejected: unauthenticated", "reason", reason, "error", err)
			// RFC 6750: tell the client how to authenticate, without details of what was wrong.
			w.Header().Set("WWW-Authenticate", `Bearer realm="orderflow", error="invalid_token"`)
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "a valid access token is required")
		})
	}
}
