package rabbitmq

import (
	"errors"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestRetryQueue(t *testing.T) {
	tests := map[time.Duration]string{
		time.Second:            "q.retry.1s",
		30 * time.Second:       "q.retry.30s",
		2 * time.Minute:        "q.retry.2m",
		90 * time.Second:       "q.retry.1m30s",
		time.Hour:              "q.retry.1h",
		500 * time.Millisecond: "q.retry.500ms",
	}
	for delay, want := range tests {
		if got := RetryQueue("q", delay); got != want {
			t.Errorf("RetryQueue(%v) = %q, want %q", delay, got, want)
		}
	}
}

func TestRetryCount(t *testing.T) {
	for _, value := range []any{int32(3), int64(3), int(3), uint8(3)} {
		if got := RetryCount(amqp.Table{HeaderRetryCount: value}); got != 3 {
			t.Errorf("RetryCount(%T) = %d, want 3", value, got)
		}
	}
	if got := RetryCount(nil); got != 0 {
		t.Errorf("RetryCount(nil) = %d, want 0", got)
	}
}

func TestRetryHeaders(t *testing.T) {
	d := amqp.Delivery{
		RoutingKey: "payment-service", // after a retry the routing key is the queue name
		Headers: amqp.Table{
			HeaderOriginalRoutingKey: "order.created",
			HeaderRetryCount:         int32(1),
			"x-death":                []any{amqp.Table{"count": int64(1)}},
			"x-first-death-queue":    "payment-service.retry.1s",
			"aggregate_id":           "42",
		},
	}
	if got := OriginalRoutingKey(d); got != "order.created" {
		t.Fatalf("OriginalRoutingKey = %q, want order.created", got)
	}

	headers := retryHeaders(d, OriginalRoutingKey(d), 2, errors.New(strings.Repeat("x", 2000)))
	if RetryCount(headers) != 2 || headers[HeaderOriginalRoutingKey] != "order.created" || headers["aggregate_id"] != "42" {
		t.Errorf("headers = %v", headers)
	}
	if _, ok := headers["x-death"]; ok {
		t.Error("x-death must be dropped")
	}
	if _, ok := headers["x-first-death-queue"]; ok {
		t.Error("x-first-death-* must be dropped")
	}
	if len(headers[HeaderLastError].(string)) > maxErrorLength+len("…") {
		t.Error("the error must be truncated")
	}

	dlq := deadLetterHeaders(d, "order.created", "payment-service", 5, errors.New("boom"))
	if dlq[HeaderError] != "boom" || dlq[HeaderOriginalQueue] != "payment-service" || RetryCount(dlq) != 5 {
		t.Errorf("dead letter headers = %v", dlq)
	}
}

func TestOriginalRoutingKey_FirstDelivery(t *testing.T) {
	if got := OriginalRoutingKey(amqp.Delivery{RoutingKey: "order.created"}); got != "order.created" {
		t.Errorf("got %q, want order.created", got)
	}
}

func TestCause_UnwrapsPermanent(t *testing.T) {
	inner := errors.New("decode order.created: invalid JSON")
	if got := cause(Permanent(inner)); got != inner {
		t.Errorf("cause(Permanent(err)) = %v, want the inner error", got)
	}
	if got := cause(inner); got != inner {
		t.Errorf("cause(err) = %v, want err itself", got)
	}
}
