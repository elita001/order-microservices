package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestInjectExtractRoundTrip(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	provider := sdktrace.NewTracerProvider()
	ctx, span := provider.Tracer("test").Start(context.Background(), "producer")
	defer span.End()

	headers := propagation.MapCarrier{}
	Inject(ctx, headers)
	if headers.Get("traceparent") == "" {
		t.Fatalf("traceparent was not injected: %v", headers)
	}

	extracted := Extract(context.Background(), headers)
	if got, want := TraceID(extracted), span.SpanContext().TraceID().String(); got != want {
		t.Errorf("trace ID after extract = %q, want %q", got, want)
	}
	if TraceID(context.Background()) != "" {
		t.Error("a context without a span must have no trace ID")
	}
}
