// Package logger configures structured JSON logging and passes a logger through context.
//
// Every record logged with a context (InfoContext and friends) gets request_id, trace_id,
// and span_id from it, so logs can be correlated across services and with traces.
package logger

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"

	"github.com/elita001/order-microservices/pkg/requestid"
)

// New creates a JSON logger that writes to stdout (Alloy collects it and sends it to Loki).
func New(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(contextHandler{slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})})
}

// WithCorrelation adds the correlation IDs of ctx to l. Use it for loggers that are
// stored in a context and used without one (logger.FromContext(ctx).Info).
func WithCorrelation(ctx context.Context, l *slog.Logger) *slog.Logger {
	if attrs := correlation(ctx); len(attrs) > 0 {
		return l.With(attrs...)
	}
	return l
}

func correlation(ctx context.Context) []any {
	var attrs []any
	if id := requestid.FromContext(ctx); id != "" {
		attrs = append(attrs, "request_id", id)
	}
	if id := requestid.ClientIDFromContext(ctx); id != "" {
		attrs = append(attrs, "client_request_id", id)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	return attrs
}

// contextHandler adds the correlation IDs of the record's context.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := correlation(ctx); len(attrs) > 0 {
		r.Add(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

type ctxKey struct{}

// WithContext stores a logger in the context (usually with the correlation IDs already added).
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext retrieves a logger from the context or returns the default logger.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
