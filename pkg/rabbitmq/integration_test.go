//go:build integration

package rabbitmq_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/elita001/order-microservices/internal/testkit"
	"github.com/elita001/order-microservices/pkg/outbox"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
	"github.com/elita001/order-microservices/pkg/requestid"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.Needs{Postgres: true, RabbitMQ: true}))
}

type received struct {
	delivery  rabbitmq.Delivery
	requestID string
	traceID   string
}

// The whole path of an event: stored in the outbox in a transaction with a request ID and
// a span, published by the relay, consumed, failed once, retried via a retry queue, and
// handled with the same request ID and trace.
func TestOutboxToConsumer_RetryKeepsRequestIDAndTrace(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	log := slog.New(slog.DiscardHandler)
	exchange, queue := "test."+uuid.NewString(), "test-"+uuid.NewString()

	var (
		mu    sync.Mutex
		got   []received
		calls int
	)
	handler := func(ctx context.Context, d rabbitmq.Delivery) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		got = append(got, received{d, requestid.FromContext(ctx), telemetry.TraceID(ctx)})
		if calls == 1 {
			return errors.New("temporary failure") // the first attempt fails
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startConsumer(ctx, t, exchange, queue, []string{"test.created"}, handler, log)

	// Store an event as a business transaction would.
	pool := testkit.Pool(t)
	traceCtx, span := otel.Tracer("test").Start(requestid.WithContext(ctx, "req-e2e-1"), "create order")
	msg, err := outbox.NewMessage("test", "42", "test.created", map[string]string{"hello": "world"})
	if err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(traceCtx, pool, func(tx pgx.Tx) error { return outbox.Store(traceCtx, tx, "order_outbox", msg) })
	span.End()
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	publisher := rabbitmq.NewPublisher(testkit.Current.RabbitMQURL, exchange, "test", log)
	defer publisher.Close()
	relay := outbox.NewRelay(pool, "order_outbox", publisher, outbox.RelayConfig{
		BatchSize: 10, PollInterval: 50 * time.Millisecond, MaxBackoff: time.Second, PublishTimeout: 5 * time.Second,
		BatchTimeout: 5 * time.Second, Retention: time.Hour, CleanupInterval: time.Hour,
	}, log)
	go func() { _ = relay.Run(ctx) }()

	waitFor(t, 15*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })

	mu.Lock()
	defer mu.Unlock()
	for i, r := range got {
		if r.delivery.RoutingKey != "test.created" || r.delivery.MessageID != msg.ID.String() {
			t.Errorf("attempt %d: routing key %q, message ID %q", i, r.delivery.RoutingKey, r.delivery.MessageID)
		}
		if r.requestID != "req-e2e-1" {
			t.Errorf("attempt %d: request ID = %q, want req-e2e-1", i, r.requestID)
		}
		if r.traceID != span.SpanContext().TraceID().String() {
			t.Errorf("attempt %d: trace ID = %q, want the producer's %s", i, r.traceID, span.SpanContext().TraceID())
		}
		if r.delivery.Retry != i {
			t.Errorf("attempt %d: retry = %d, want %d", i, r.delivery.Retry, i)
		}
	}
}

// A permanent error moves the message to the DLQ at once, with the error in its headers.
func TestConsumer_PermanentErrorGoesToDLQ(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	exchange, queue := "test."+uuid.NewString(), "test-"+uuid.NewString()
	var calls int32
	var mu sync.Mutex
	handler := func(context.Context, rabbitmq.Delivery) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return rabbitmq.Permanent(errors.New("malformed message"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startConsumer(ctx, t, exchange, queue, []string{"test.bad"}, handler, log)

	publisher := rabbitmq.NewPublisher(testkit.Current.RabbitMQURL, exchange, "test", log)
	defer publisher.Close()
	msg, _ := outbox.NewMessage("test", "1", "test.bad", map[string]string{})
	if err := publisher.Publish(ctx, msg); err != nil {
		t.Fatal(err)
	}

	conn, err := rabbitmq.Dial(testkit.Current.RabbitMQURL, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	var dead amqp.Delivery
	waitFor(t, 10*time.Second, func() bool {
		d, ok, err := ch.Get(rabbitmq.DeadLetterQueue(queue), true)
		dead = d
		return err == nil && ok
	})
	if dead.MessageId != msg.ID.String() || dead.Headers[rabbitmq.HeaderError] != "malformed message" {
		t.Errorf("dead-lettered message: id %q, headers %v", dead.MessageId, dead.Headers)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("handler called %d times, want 1: a permanent error must not be retried", calls)
	}
}

func startConsumer(ctx context.Context, t *testing.T, exchange, queue string, keys []string, h rabbitmq.Handler, log *slog.Logger) {
	t.Helper()
	consumer := rabbitmq.NewConsumer(testkit.Current.RabbitMQURL, "test", rabbitmq.Topology{
		Exchange: exchange, Queue: queue, RoutingKeys: keys,
	}, rabbitmq.ConsumerConfig{
		Prefetch: 10, Concurrency: 1, HandlerTimeout: 5 * time.Second,
		RetryDelays:  []time.Duration{200 * time.Millisecond},
		RequeueDelay: 100 * time.Millisecond, MaxBackoff: time.Second, HealthInterval: time.Second,
	}, log)
	go func() { _ = consumer.Run(ctx, h) }()

	// The consumer declares the queue and its bindings on connect; wait until it is bound.
	conn, err := rabbitmq.Dial(testkit.Current.RabbitMQURL, "test-wait")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitFor(t, 10*time.Second, func() bool {
		ch, err := conn.Channel()
		if err != nil {
			return false
		}
		defer ch.Close()
		q, err := ch.QueueDeclarePassive(queue, true, false, false, false, amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum})
		return err == nil && q.Consumers == 1
	})
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}
