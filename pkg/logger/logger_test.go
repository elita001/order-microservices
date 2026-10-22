package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/elita001/order-microservices/pkg/requestid"
)

func TestContextHandlerAddsCorrelationIDs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(contextHandler{slog.NewJSONHandler(&buf, nil)})

	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "op")
	defer span.End()
	ctx = requestid.WithContext(ctx, "req-1")

	log.InfoContext(ctx, "with context")
	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["request_id"] != "req-1" || record["trace_id"] != span.SpanContext().TraceID().String() || record["span_id"] == nil {
		t.Errorf("record = %v, want request_id, trace_id, span_id", record)
	}

	buf.Reset()
	log.Info("without context")
	if bytes.Contains(buf.Bytes(), []byte("request_id")) {
		t.Errorf("a record without context must not get correlation IDs: %s", buf.String())
	}
}
