package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/customer/domain"
)

type fakeRepo struct {
	customers map[uuid.UUID]domain.Customer
}

func (r *fakeRepo) Get(_ context.Context, id uuid.UUID) (*domain.Customer, error) {
	c, ok := r.customers[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &c, nil
}

func (r *fakeRepo) Save(_ context.Context, c *domain.Customer) error {
	r.customers[c.ID] = *c
	return nil
}

func (r *fakeRepo) Delete(_ context.Context, id uuid.UUID) error {
	delete(r.customers, id)
	return nil
}

type fakeDirectory struct {
	users map[uuid.UUID]domain.Customer
	err   error
	calls int
}

func (d *fakeDirectory) User(_ context.Context, id uuid.UUID) (*domain.Customer, error) {
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	u, ok := d.users[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &u, nil
}

type nopLogger struct{}

func (nopLogger) InfoContext(context.Context, string, ...any) {}
func (nopLogger) WarnContext(context.Context, string, ...any) {}

type recordedLookups []string

func (r *recordedLookups) CustomerLookup(result string) { *r = append(*r, result) }

var now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func newTestService(t *testing.T) (*Service, *fakeRepo, *fakeDirectory, *recordedLookups) {
	t.Helper()
	repo := &fakeRepo{customers: map[uuid.UUID]domain.Customer{}}
	dir := &fakeDirectory{users: map[uuid.UUID]domain.Customer{}}
	lookups := &recordedLookups{}
	svc := NewService(repo, dir, Config{RefreshAfter: time.Hour}, nopLogger{}).WithMetrics(lookups)
	svc.now = func() time.Time { return now }
	return svc, repo, dir, lookups
}

var support = Viewer{SeeAll: true}

func TestGet_FetchesFromDirectoryAndStoresCopy(t *testing.T) {
	svc, repo, dir, lookups := newTestService(t)
	id := uuid.New()
	dir.users[id] = domain.Customer{ID: id, Username: "alice", Email: "alice@example.com"}

	c, err := svc.Get(context.Background(), support, id)
	if err != nil || c.Email != "alice@example.com" || !c.SyncedAt.Equal(now) {
		t.Fatalf("got %+v, %v", c, err)
	}
	if stored := repo.customers[id]; stored.Email != "alice@example.com" {
		t.Errorf("stored copy = %+v", stored)
	}

	// A recent copy is served without asking the directory again.
	if _, err := svc.Get(context.Background(), support, id); err != nil {
		t.Fatal(err)
	}
	if dir.calls != 1 {
		t.Errorf("directory called %d times, want 1", dir.calls)
	}
	if want := []string{LookupRefreshed, LookupFresh}; !equal(*lookups, want) {
		t.Errorf("lookups = %v, want %v", *lookups, want)
	}
}

func TestGet_RefreshesOldCopy(t *testing.T) {
	svc, repo, dir, _ := newTestService(t)
	id := uuid.New()
	repo.customers[id] = domain.Customer{ID: id, Email: "old@example.com", SyncedAt: now.Add(-2 * time.Hour)}
	dir.users[id] = domain.Customer{ID: id, Email: "new@example.com"}

	c, err := svc.Get(context.Background(), support, id)
	if err != nil || c.Email != "new@example.com" {
		t.Fatalf("got %+v, %v", c, err)
	}
	if repo.customers[id].Email != "new@example.com" {
		t.Error("the copy was not refreshed")
	}
}

// Keycloak is down: an old copy is better than no email at all.
func TestGet_ServesStaleCopyWhenDirectoryFails(t *testing.T) {
	svc, repo, dir, lookups := newTestService(t)
	id := uuid.New()
	repo.customers[id] = domain.Customer{ID: id, Email: "old@example.com", SyncedAt: now.Add(-2 * time.Hour)}
	dir.err = errors.New("connection refused")

	c, err := svc.Get(context.Background(), support, id)
	if err != nil || c.Email != "old@example.com" {
		t.Fatalf("got %+v, %v", c, err)
	}
	if (*lookups)[0] != LookupStale {
		t.Errorf("lookups = %v", *lookups)
	}
}

func TestGet_DirectoryFailsWithoutCopy(t *testing.T) {
	svc, _, dir, _ := newTestService(t)
	dir.err = errors.New("connection refused")

	_, err := svc.Get(context.Background(), support, uuid.New())
	if !errors.Is(err, domain.ErrDirectoryUnavailable) {
		t.Fatalf("got %v, want ErrDirectoryUnavailable", err)
	}
}

// A user deleted in Keycloak: the copy of their personal data is deleted too.
func TestGet_DeletedUserIsRemoved(t *testing.T) {
	svc, repo, _, _ := newTestService(t)
	id := uuid.New()
	repo.customers[id] = domain.Customer{ID: id, Email: "gone@example.com", SyncedAt: now.Add(-2 * time.Hour)}

	if _, err := svc.Get(context.Background(), support, id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if _, ok := repo.customers[id]; ok {
		t.Error("the copy of a deleted user was kept")
	}
}

func TestGet_CustomersSeeOnlyThemselves(t *testing.T) {
	svc, _, dir, _ := newTestService(t)
	alice, bob := uuid.New(), uuid.New()
	dir.users[alice] = domain.Customer{ID: alice}
	dir.users[bob] = domain.Customer{ID: bob}

	if _, err := svc.Get(context.Background(), Viewer{CustomerID: alice}, alice); err != nil {
		t.Errorf("own profile: %v", err)
	}
	if _, err := svc.Get(context.Background(), Viewer{CustomerID: alice}, bob); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another customer's profile: %v, want ErrNotFound", err)
	}
	if dir.calls != 1 {
		t.Errorf("directory called %d times: another customer's profile must not be looked up", dir.calls)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
