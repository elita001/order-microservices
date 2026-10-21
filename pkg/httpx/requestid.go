package httpx

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

// RequestIDHeader carries the request ID between the client, the gateway, and the services.
// It matches the header that chi's middleware.RequestID reads in the services.
const RequestIDHeader = "X-Request-Id"

const maxRequestIDLength = 64

// RequestID is the edge (gateway) version of chi's middleware.RequestID. It accepts the
// client's X-Request-Id only if it is short and safe to log, otherwise it generates a new
// one. The ID is stored where middleware.GetReqID finds it, returned in the response,
// and kept in the request headers, so the proxy forwards it and the logs of all services
// share one request_id.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = uuid.NewString()
			r.Header.Set(RequestIDHeader, id)
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), middleware.RequestIDKey, id)
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
