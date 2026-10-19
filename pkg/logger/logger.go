// Package logger configures structured JSON logging
// and allows passing a logger through context.
package logger

import (
	"context"
	"log/slog"
	"os"
)

// New creates a JSON logger that writes to stdout (Alloy -> Loki collects the logs from there).
func New(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

type ctxKey struct{}

// WithContext stores a logger in the context (usually with request_id already added).
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
