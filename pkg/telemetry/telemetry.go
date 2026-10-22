// Package telemetry sets up OpenTelemetry tracing: spans are exported over OTLP to the
// collector (Grafana Alloy), which forwards them to Tempo. Trace context travels in the
// W3C traceparent header: over HTTP between the gateway and the services, and in outbox
// and RabbitMQ message headers between the services.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Config is usually loaded from the standard OpenTelemetry environment variables.
type Config struct {
	// OTLP/HTTP collector address, e.g. http://alloy:4318. Empty disables tracing.
	Endpoint    string  `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	SampleRatio float64 `env:"OTEL_TRACES_SAMPLER_RATIO" envDefault:"1"` // share of traces kept, 0..1
}

// Setup installs the global tracer provider and propagator. The returned function
// flushes buffered spans; call it on shutdown.
func Setup(ctx context.Context, serviceName string, cfg Config) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil // the default no-op provider
	}

	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(strings.TrimSuffix(cfg.Endpoint, "/")+"/v1/traces"))
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter), // spans are sent in the background, not on the request path
		sdktrace.WithResource(res),
		// Follow the caller's decision, so a trace is either complete or absent.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// Tracer returns a named tracer of the global provider.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// HTTPHandler starts a server span for every request, continuing the caller's trace if
// the request has a traceparent header. Probes and metrics scrapes are not traced.
func HTTPHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "http.server", otelhttp.WithFilter(traced))
}

// PublicHTTPHandler is HTTPHandler for the edge of the system (the gateway). It always
// starts a new trace: a traceparent from the internet is only linked to the server span,
// never its parent. Otherwise a client could attach requests to someone else's trace or
// set the "sampled" flag to force every one of its traces to be stored.
func PublicHTTPHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "http.server", otelhttp.WithFilter(traced),
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }))
}

func traced(r *http.Request) bool {
	switch r.URL.Path {
	case "/metrics", "/healthz", "/readyz":
		return false
	}
	return true
}

// HTTPTransport traces outgoing requests and adds the traceparent header to them.
func HTTPTransport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base)
}

// RouteName names the server span after the chi route pattern ("GET /api/v1/orders/{id}"),
// so that traces are grouped by endpoint, not by order ID. It must be used on a chi router.
func RouteName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		span := trace.SpanFromContext(r.Context())
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
			span.SetName(r.Method + " " + rctx.RoutePattern())
			span.SetAttributes(semconv.HTTPRoute(rctx.RoutePattern()))
		}
	})
}

// Inject writes the trace context of ctx into a string carrier (outbox or AMQP headers).
func Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	otel.GetTextMapPropagator().Inject(ctx, carrier)
}

// Extract returns ctx with the trace context read from a carrier.
func Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// TraceID returns the trace ID of ctx, or "" if ctx has no valid span.
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// MessagingAttributes describes a RabbitMQ message on producer and consumer spans.
func MessagingAttributes(destination, messageID string) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.MessagingSystemRabbitMQ,
		semconv.MessagingDestinationName(destination),
		semconv.MessagingMessageID(messageID),
	}
}
