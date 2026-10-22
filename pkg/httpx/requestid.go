package httpx

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/elita001/order-microservices/pkg/requestid"
)

// RequestIDHeader carries the request ID between the client, the gateway, and the services.
// It matches the header that chi's middleware.RequestID reads in the services.
const RequestIDHeader = "X-Request-Id"

const maxRequestIDLength = 64

// RequestID is the edge (gateway) version of chi's middleware.RequestID. It always
// generates a new ID: a client-supplied one could collide with another client's or be
// chosen to mix into someone else's logs. The ID is returned in the response, so the
// client can quote it to support, and forwarded to the services, so the logs of all
// services share one request_id.
//
// A client's own X-Request-Id is kept as client_request_id in the logs if it is short and
// safe to log, so the client's logs can still be matched with ours.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if clientID := r.Header.Get(RequestIDHeader); validRequestID(clientID) {
			ctx = requestid.WithClientID(ctx, clientID)
		}

		id := uuid.NewString()
		r.Header.Set(RequestIDHeader, id) // the proxy forwards ours, not the client's
		w.Header().Set(RequestIDHeader, id)
		ctx = context.WithValue(ctx, middleware.RequestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range id {
		isAlnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !isAlnum && c != '-' && c != '_' && c != '.' && c != ':' {
			return false
		}
	}
	return true
}
