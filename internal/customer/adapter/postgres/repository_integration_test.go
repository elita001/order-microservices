//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	customerpostgres "github.com/elita001/order-microservices/internal/customer/adapter/postgres"
	"github.com/elita001/order-microservices/internal/customer/domain"
	"github.com/elita001/order-microservices/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Main(m, testkit.Needs{Postgres: true})) }

func TestRepository_SaveGetDelete(t *testing.T) {
	repo := customerpostgres.NewRepository(testkit.Pool(t))
	ctx := context.Background()
	id := uuid.New()

	if _, err := repo.Get(ctx, id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get missing: %v, want ErrNotFound", err)
	}

	synced := time.Now().UTC().Truncate(time.Microsecond)
	c := &domain.Customer{ID: id, Username: "alice", Email: "alice@example.com", FirstName: "Alice", SyncedAt: synced}
	if err := repo.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Saving again replaces the copy (a refresh from the identity provider).
	c.Email, c.SyncedAt = "alice@new.example.com", synced.Add(time.Hour)
	if err := repo.Save(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "alice@new.example.com" || got.Username != "alice" || !got.SyncedAt.Equal(synced.Add(time.Hour)) {
		t.Errorf("got %+v", got)
	}

	if err := repo.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, id); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get deleted: %v, want ErrNotFound", err)
	}
}
