//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	orderpostgres "github.com/elita001/order-microservices/internal/order/adapter/postgres"
	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/internal/testkit"
	"github.com/elita001/order-microservices/pkg/requestid"
)

func TestMain(m *testing.M) { os.Exit(testkit.Main(m, testkit.Needs{Postgres: true})) }

func newOrder(customerID uuid.UUID, key string, createdAt time.Time) *domain.Order {
	return &domain.Order{
		ID: uuid.New(), CustomerID: customerID, Status: domain.StatusPending,
		Items:       []domain.Item{{ProductID: "book", Quantity: 2, Price: 100}, {ProductID: "pen", Quantity: 1, Price: 50}},
		TotalAmount: 250, Currency: "RUB",
		CreatedAt: createdAt.UTC().Truncate(time.Microsecond), UpdatedAt: createdAt.UTC().Truncate(time.Microsecond),
		IdempotencyKey: key, RequestHash: "hash-" + key,
	}
}

func TestRepository_CreateStoresOrderItemsAndOutboxEvent(t *testing.T) {
	pool := testkit.Pool(t)
	repo := orderpostgres.NewRepository(pool)
	ctx := requestid.WithContext(context.Background(), "req-integration-1")
	order := newOrder(uuid.New(), "", time.Now())

	if err := repo.Create(ctx, order, domain.NewOrderCreated(order)); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TotalAmount != 250 || len(got.Items) != 2 || got.Items[0].ProductID != "book" || got.Status != domain.StatusPending {
		t.Errorf("stored order = %+v", got)
	}

	// The event is in the same transaction, with the request ID in its headers.
	var (
		eventType string
		headers   map[string]string
	)
	err = pool.QueryRow(ctx, `SELECT event_type, headers FROM order_outbox WHERE aggregate_id = $1`, order.ID.String()).
		Scan(&eventType, &headers)
	if err != nil {
		t.Fatalf("select outbox row: %v", err)
	}
	if eventType != "order.created" || headers[requestid.MessageHeader] != "req-integration-1" {
		t.Errorf("outbox row: type %q, headers %v", eventType, headers)
	}
}

func TestRepository_DuplicateIdempotencyKey(t *testing.T) {
	repo := orderpostgres.NewRepository(testkit.Pool(t))
	ctx := context.Background()
	key := uuid.NewString()
	first := newOrder(uuid.New(), key, time.Now())

	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := repo.Create(ctx, newOrder(first.CustomerID, key, time.Now()))
	if !errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		t.Fatalf("second create with the same key: got %v, want ErrDuplicateIdempotencyKey", err)
	}

	got, err := repo.GetByIdempotencyKey(ctx, first.CustomerID, key)
	if err != nil || got.ID != first.ID || got.RequestHash != first.RequestHash {
		t.Fatalf("GetByIdempotencyKey = %+v, %v; want the first order", got, err)
	}
	// The same key of another customer is a different key.
	if err := repo.Create(ctx, newOrder(uuid.New(), key, time.Now())); err != nil {
		t.Fatalf("another customer with the same key: %v", err)
	}
	if _, err := repo.GetByIdempotencyKey(ctx, uuid.New(), key); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another customer's lookup: got %v, want ErrNotFound", err)
	}
	// Orders without a key do not conflict with each other (NULLs are not equal).
	for range 2 {
		if err := repo.Create(ctx, newOrder(uuid.New(), "", time.Now())); err != nil {
			t.Fatalf("create without a key: %v", err)
		}
	}
}

func TestRepository_UpdateStatusOnlyFromExpectedStatuses(t *testing.T) {
	repo := orderpostgres.NewRepository(testkit.Pool(t))
	ctx := context.Background()
	order := newOrder(uuid.New(), "", time.Now())
	if err := repo.Create(ctx, order); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		from    []domain.Status
		to      domain.Status
		updated bool
	}{
		{[]domain.Status{domain.StatusPending}, domain.StatusPaid, true},
		{[]domain.Status{domain.StatusPending}, domain.StatusPaid, false}, // a repeated event changes nothing
		{[]domain.Status{domain.StatusPending}, domain.StatusPaymentFailed, false},
		{[]domain.Status{domain.StatusPending, domain.StatusPaid}, domain.StatusShipped, true},
	}
	for i, step := range steps {
		updated, err := repo.UpdateStatus(ctx, order.ID, step.from, step.to, time.Now())
		if err != nil || updated != step.updated {
			t.Fatalf("step %d: updated = %v, %v; want %v", i, updated, err, step.updated)
		}
	}
	got, _ := repo.GetByID(ctx, order.ID)
	if got.Status != domain.StatusShipped {
		t.Errorf("final status = %q, want shipped", got.Status)
	}
	if updated, err := repo.UpdateStatus(ctx, uuid.New(), []domain.Status{domain.StatusPending}, domain.StatusPaid, time.Now()); updated || err != nil {
		t.Errorf("unknown order: updated = %v, %v; want false, nil", updated, err)
	}
}

func TestRepository_ListByCustomerNewestFirst(t *testing.T) {
	repo := orderpostgres.NewRepository(testkit.Pool(t))
	ctx := context.Background()
	customer := uuid.New()
	start := time.Now().Add(-time.Hour)
	var ids []uuid.UUID
	for i := range 5 {
		order := newOrder(customer, "", start.Add(time.Duration(i)*time.Minute))
		ids = append(ids, order.ID)
		if err := repo.Create(ctx, order); err != nil {
			t.Fatal(err)
		}
	}

	page, err := repo.ListByCustomer(ctx, customer, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != ids[3] || page[1].ID != ids[2] {
		t.Errorf("page = %v, want orders 3 and 2 (newest first, offset 1)", []uuid.UUID{page[0].ID, page[1].ID})
	}
	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown order: got %v, want ErrNotFound", err)
	}
}
