//go:build integration

package postgres_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	paymentpostgres "github.com/elita001/order-microservices/internal/payment/adapter/postgres"
	"github.com/elita001/order-microservices/internal/payment/domain"
	"github.com/elita001/order-microservices/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Main(m, testkit.Needs{Postgres: true})) }

func pending(orderID uuid.UUID) *domain.Payment {
	now := time.Now().UTC()
	return &domain.Payment{ID: uuid.New(), OrderID: orderID, CustomerID: uuid.New(), Amount: 500, Currency: "RUB",
		Status: domain.StatusPending, CreatedAt: now, UpdatedAt: now}
}

// Concurrent deliveries of the same order.created must create one payment and publish
// one result event: the unique order_id and the status check in UPDATE guarantee it.
func TestRepository_ConcurrentDeliveriesCompleteOnce(t *testing.T) {
	pool := testkit.Pool(t)
	repo := paymentpostgres.NewRepository(pool)
	ctx := context.Background()
	orderID := uuid.New()

	const deliveries = 10
	var (
		wg        sync.WaitGroup
		completed atomic.Int32
		paymentID sync.Map
	)
	for range deliveries {
		wg.Go(func() {
			p, err := repo.GetOrCreate(ctx, pending(orderID))
			if err != nil {
				t.Errorf("get or create: %v", err)
				return
			}
			paymentID.Store(p.ID, true)
			event, err := p.Succeed("tx-1", time.Now().UTC())
			if err != nil {
				return // already completed in memory
			}
			ok, err := repo.Complete(ctx, p, event)
			if err != nil {
				t.Errorf("complete: %v", err)
			}
			if ok {
				completed.Add(1)
			}
		})
	}
	wg.Wait()

	ids := 0
	paymentID.Range(func(any, any) bool { ids++; return true })
	if ids != 1 {
		t.Errorf("deliveries saw %d different payments, want 1", ids)
	}
	if completed.Load() != 1 {
		t.Errorf("payment completed %d times, want 1", completed.Load())
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_outbox WHERE payload->>'order_id' = $1`, orderID.String()).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("payment events = %d, want 1", events)
	}
}
