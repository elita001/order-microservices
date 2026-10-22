// Package errtrack reports errors that need a human to look at them (panics, unexpected
// 500s, messages moved to a dead letter queue) to Sentry or any Sentry-compatible server,
// such as GlitchTip. Each report carries the request ID and the trace ID, which lead to
// the logs in Loki and the trace in Tempo.
//
// Expected conditions (validation errors, an unavailable database, retried messages) are
// not reported: metrics and alerts cover them, and reporting them would bury real bugs.
package errtrack

import (
	"context"
	"log/slog"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/elita001/order-microservices/pkg/requestid"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

// Config is usually loaded from environment variables.
type Config struct {
	DSN         string `env:"SENTRY_DSN"` // empty disables reporting
	Environment string `env:"SENTRY_ENVIRONMENT" envDefault:"development"`
	Release     string `env:"SENTRY_RELEASE"`
}

var enabled bool

// Init configures the global client. The returned function sends buffered events;
// call it on shutdown.
func Init(service string, cfg Config, log *slog.Logger) (flush func(), err error) {
	if cfg.DSN == "" {
		log.Info("error tracking is disabled: SENTRY_DSN is not set")
		return func() {}, nil
	}
	err = sentry.Init(sentry.ClientOptions{
		Dsn:              cfg.DSN,
		Environment:      cfg.Environment,
		Release:          cfg.Release,
		ServerName:       service,
		AttachStacktrace: true,
		SendDefaultPII:   false, // no client IPs or request bodies in reports
	})
	if err != nil {
		return nil, err
	}
	sentry.ConfigureScope(func(scope *sentry.Scope) { scope.SetTag("service", service) })
	enabled = true
	return func() { sentry.Flush(2 * time.Second) }, nil
}

// CaptureError reports err with the correlation IDs from ctx and extra tags.
func CaptureError(ctx context.Context, err error, tags map[string]string) {
	if !enabled || err == nil {
		return
	}
	withScope(ctx, tags, func(hub *sentry.Hub) { hub.CaptureException(err) })
}

// CapturePanic reports a recovered panic with its stack trace.
func CapturePanic(ctx context.Context, recovered any, tags map[string]string) {
	if !enabled {
		return
	}
	withScope(ctx, tags, func(hub *sentry.Hub) { hub.RecoverWithContext(ctx, recovered) })
}

func withScope(ctx context.Context, tags map[string]string, capture func(hub *sentry.Hub)) {
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		if id := requestid.FromContext(ctx); id != "" {
			scope.SetTag("request_id", id)
		}
		if id := telemetry.TraceID(ctx); id != "" {
			scope.SetTag("trace_id", id)
		}
		for key, value := range tags {
			scope.SetTag(key, value)
		}
		capture(hub)
	})
}
