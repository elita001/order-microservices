package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

// The gateway must not continue a trace started by a client from the internet.
func TestPublicHTTPHandlerStartsNewTrace(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	const clientTraceID = "0af7651916cd43dd8448eb211c80319c"
	for name, handler := range map[string]http.Handler{
		"internal": HTTPHandler(http.NotFoundHandler()),
		"public":   PublicHTTPHandler(http.NotFoundHandler()),
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
		req.Header.Set("traceparent", "00-"+clientTraceID+"-b7ad6b7169203331-01")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		spans := recorder.Ended()
		span := spans[len(spans)-1]
		continued := span.SpanContext().TraceID().String() == clientTraceID
		switch name {
		case "internal":
			if !continued {
				t.Error("an internal service must continue the caller's trace")
			}
		case "public":
			if continued {
				t.Error("the public endpoint must start a new trace")
			}
			if len(span.Links()) != 1 || span.Links()[0].SpanContext.TraceID().String() != clientTraceID {
				t.Errorf("links = %v, want a link to the client's trace", span.Links())
			}
		}
	}
}
