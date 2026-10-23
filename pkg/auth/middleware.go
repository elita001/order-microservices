package auth

import (
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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
				var p Principal
				if p, err = v.Verify(r.Context(), raw); err == nil {
					// The user ID is pseudonymous (a UUID), fine for logs and traces.
					trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("enduser.id", p.UserID.String()))
					ctx := WithPrincipal(r.Context(), p)
					// Records logged with ctx (the use cases) and with the request logger
					// (the adapters) both get the user.
					ctx = logger.WithAttrs(ctx, "user_id", p.UserID.String())
					ctx = logger.WithContext(ctx, logger.FromContext(ctx).With("user_id", p.UserID.String()))
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			reason := "invalid_token"
			if errors.Is(err, ErrMissingToken) {
				reason = "missing_token"
			}
			if onFailure != nil {
				onFailure(reason)
			}
			logger.FromContext(r.Context()).Info("request rejected: unauthenticated", "reason", reason, "error", err)
			// RFC 6750: tell the client how to authenticate, without details of what was wrong.
			w.Header().Set("WWW-Authenticate", `Bearer realm="orderflow", error="invalid_token"`)
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "a valid access token is required")
		})
	}
}
