package gateway

import (
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/elita001/order-microservices/pkg/grpcx"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
)

// The reason the order service gives for a reused idempotency key
// (see proto/orderflow/order/v1/order.proto).
const reasonIdempotencyKeyReused = "IDEMPOTENCY_KEY_REUSED"

// writeUpstreamError maps a gRPC error of a service to the REST API's error response.
//
// UNAVAILABLE is ambiguous: with an ErrorInfo reason, the service itself answered that a
// dependency is down (503, retry later); without one, gRPC could not reach the service (502).
func writeUpstreamError(w http.ResponseWriter, r *http.Request, upstream string, err error) {
	st := status.Convert(err)
	log := logger.FromContext(r.Context()).With("upstream", upstream, "grpc_code", st.Code().String())

	switch st.Code() {
	case codes.InvalidArgument:
		code := "validation_error"
		if grpcx.Reason(err) == reasonIdempotencyKeyReused {
			code = "idempotency_key_reused"
		}
		httpx.WriteError(w, http.StatusUnprocessableEntity, code, st.Message())
	case codes.NotFound:
		httpx.WriteError(w, http.StatusNotFound, "not_found", st.Message())
	case codes.PermissionDenied:
		httpx.WriteError(w, http.StatusForbidden, "forbidden", st.Message())
	case codes.Unauthenticated:
		// The gateway checked the token, so the service disagrees (e.g., it expired in between).
		w.Header().Set("WWW-Authenticate", `Bearer realm="orderflow", error="invalid_token"`)
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "a valid access token is required")
	case codes.Canceled:
		// The client has gone away; nobody will read the response.
		log.Info("client closed request", "error", st.Message())
	case codes.DeadlineExceeded:
		log.Error("upstream timeout", "error", st.Message())
		httpx.WriteError(w, http.StatusGatewayTimeout, "upstream_timeout", "upstream service did not respond in time")
	case codes.Unavailable:
		if grpcx.Reason(err) != "" {
			log.Warn("upstream temporarily unavailable", "reason", grpcx.Reason(err))
			w.Header().Set("Retry-After", "5")
			httpx.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", st.Message())
			return
		}
		log.Error("upstream unavailable", "error", st.Message())
		httpx.WriteError(w, http.StatusBadGateway, "upstream_unavailable", "upstream service is unavailable")
	default:
		// INTERNAL and anything unexpected: the service has already reported its own errors.
		log.Error("upstream call failed", "error", st.Message())
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
