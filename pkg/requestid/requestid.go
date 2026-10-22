// Package requestid carries the request ID (a correlation ID) through a business flow:
// from the HTTP request that started it, via the outbox and RabbitMQ headers, to every
// service that handles the resulting events. Searching the logs by one request_id shows
// the whole life of an order across all services.
package requestid

import (
	"context"

	"github.com/go-chi/chi/v5/middleware"
)

// MessageHeader carries the request ID in outbox and AMQP message headers.
const MessageHeader = "x-request-id"

type ctxKey struct{}

func WithContext(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request ID set by WithContext or by chi's RequestID middleware.
func FromContext(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKey{}).(string); ok {
		return id
	}
	return middleware.GetReqID(ctx)
}
