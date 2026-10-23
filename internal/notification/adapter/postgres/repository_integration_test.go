//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	notificationpostgres "github.com/elita001/order-microservices/internal/notification/adapter/postgres"
	"github.com/elita001/order-microservices/internal/notification/domain"
	"github.com/elita001/order-microservices/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Main(m, testkit.Needs{Postgres: true})) }

func notification(orderID uuid.UUID) *domain.Notification {
	return &domain.Notification{ID: uuid.New(), Kind: domain.KindOrderCreated, OrderID: orderID, CustomerID: uuid.New(),
		Recipient: "customer@example.com", Subject: "Order received", SentAt: time.Now().UTC()}
}

// Concurrent duplicates wait on the unique (kind, order_id) index: the email is sent once.
func TestRepository_SendOnceUnderConcurrency(t *testing.T) {
	repo := notificationpostgres.NewRepository(testkit.Pool(t))
	ctx := context.Background()
	orderID := uuid.New()

	var sends atomic.Int32
	send := func(context.Context) error {
		sends.Add(1)
		time.Sleep(50 * time.Millisecond) // a slow SMTP server keeps the transaction open
		return nil
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := repo.SendOnce(ctx, notification(orderID), send); err != nil {
				t.Errorf("send once: %v", err)
			}
		})
	}
	wg.Wait()

	if sends.Load() != 1 {
		t.Errorf("email sent %d times, want 1", sends.Load())
	}
	if sent, err := repo.Sent(ctx, domain.KindOrderCreated, orderID); err != nil || !sent {
		t.Errorf("Sent = %v, %v; want true", sent, err)
	}
}

// A failed send rolls the record back, so a retry sends the email.
func TestRepository_FailedSendCanBeRetried(t *testing.T) {
	repo := notificationpostgres.NewRepository(testkit.Pool(t))
	ctx := context.Background()
	orderID := uuid.New()
	smtpDown := errors.New("smtp is down")

	if _, err := repo.SendOnce(ctx, notification(orderID), func(context.Context) error { return smtpDown }); !errors.Is(err, smtpDown) {
		t.Fatalf("got %v, want the send error", err)
	}
	if sent, _ := repo.Sent(ctx, domain.KindOrderCreated, orderID); sent {
		t.Fatal("a failed send must not be recorded")
	}
	sent, err := repo.SendOnce(ctx, notification(orderID), func(context.Context) error { return nil })
	if err != nil || !sent {
		t.Fatalf("retry: sent = %v, %v; want true", sent, err)
	}
}
