package cache

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/pkg/breaker"
	"github.com/elita001/order-microservices/pkg/redisclient"
)

// fakeDB is the wrapped repository: it counts reads and can delay them.
type fakeDB struct {
	application.Repository // other methods are not used

	mu     sync.Mutex
	orders map[uuid.UUID]domain.Order
	reads  atomic.Int32
	delay  time.Duration
}

func (f *fakeDB) GetByID(_ context.Context, id uuid.UUID) (*domain.Order, error) {
	f.reads.Add(1)
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	order, ok := f.orders[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &order, nil
}

func (f *fakeDB) UpdateStatus(_ context.Context, id uuid.UUID, _ []domain.Status, to domain.Status, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	order := f.orders[id]
	order.Status, order.UpdatedAt = to, at.Truncate(time.Microsecond)
	f.orders[id] = order
	return true, nil
}

type testEnv struct {
	db    *fakeDB
	redis *miniredis.Miniredis
	cache *Repository
	order domain.Order
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb, err := redisclient.New(redisclient.Config{
		URL: "redis://" + mr.Addr(), DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		Breaker: breaker.Config{Failures: 3, OpenTimeout: time.Minute, HalfOpenRequests: 1},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	order := domain.Order{
		ID: uuid.New(), CustomerID: uuid.New(), Status: domain.StatusPending,
		Items:       []domain.Item{{ProductID: "book", Quantity: 2, Price: 100}},
		TotalAmount: 200, Currency: "RUB",
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond), UpdatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	db := &fakeDB{orders: map[uuid.UUID]domain.Order{order.ID: order}}
	return &testEnv{
		db:    db,
		redis: mr,
		cache: New(db, rdb, Config{TTL: time.Minute, LoadTimeout: time.Second}, 100*time.Millisecond, slog.New(slog.DiscardHandler)),
		order: order,
	}
}

func (env *testEnv) get(t *testing.T) *domain.Order {
	t.Helper()
	order, err := env.cache.GetByID(context.Background(), env.order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	return order
}

func TestCache_MissThenHit(t *testing.T) {
	env := newTestEnv(t)

	first, second := env.get(t), env.get(t)
	if reads := env.db.reads.Load(); reads != 1 {
		t.Errorf("database read %d times, want 1", reads)
	}
	if first.ID != env.order.ID || second.Status != domain.StatusPending || len(second.Items) != 1 {
		t.Errorf("cached order = %+v, want %+v", second, env.order)
	}
	if ttl := env.redis.TTL(keyPrefix + env.order.ID.String()); ttl < time.Minute || ttl > 66*time.Second {
		t.Errorf("ttl = %v, want 1m plus up to 10%% jitter", ttl)
	}
}

func TestCache_StatusChangeInvalidates(t *testing.T) {
	env := newTestEnv(t)
	env.get(t)

	if _, err := env.cache.UpdateStatus(context.Background(), env.order.ID, nil, domain.StatusPaid, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := env.get(t).Status; got != domain.StatusPaid {
		t.Errorf("status after update = %q, want paid", got)
	}
	if got := env.get(t).Status; got != domain.StatusPaid {
		t.Errorf("status from cache = %q, want paid", got)
	}
	if reads := env.db.reads.Load(); reads != 2 {
		t.Errorf("database read %d times, want 2 (before and after the update)", reads)
	}
}

// A reader that loaded the order before a status change must not put it back into the
// cache after the invalidation.
func TestCache_StaleWriteAfterInvalidationIsRejected(t *testing.T) {
	env := newTestEnv(t)
	stale := env.order // read from the database before the update

	if _, err := env.cache.UpdateStatus(context.Background(), env.order.ID, nil, domain.StatusPaid, time.Now()); err != nil {
		t.Fatal(err)
	}
	env.cache.set(context.Background(), &stale) // the slow reader finishes now

	if got := env.get(t).Status; got != domain.StatusPaid {
		t.Errorf("status = %q, want paid: the stale write must be rejected", got)
	}
}

func TestCache_ConcurrentMissesShareOneQuery(t *testing.T) {
	env := newTestEnv(t)
	env.db.delay = 50 * time.Millisecond

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { env.get(t) })
	}
	wg.Wait()
	if reads := env.db.reads.Load(); reads != 1 {
		t.Errorf("database read %d times, want 1", reads)
	}
}

// The shared load must not fail for everyone when the caller that started it gives up.
func TestCache_CanceledFirstCallerDoesNotFailOthers(t *testing.T) {
	env := newTestEnv(t)
	env.db.delay = 100 * time.Millisecond

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := env.cache.GetByID(firstCtx, env.order.ID)
		firstErr <- err
	}()
	time.Sleep(20 * time.Millisecond) // the first caller has started the shared load
	cancelFirst()

	if got := env.get(t); got.ID != env.order.ID { // joins the same load
		t.Fatalf("got order %s", got.ID)
	}
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Errorf("first caller: got %v, want context.Canceled", err)
	}
	if reads := env.db.reads.Load(); reads != 1 {
		t.Errorf("database read %d times, want 1", reads)
	}
}

// After a few failures the breaker opens, and requests stop waiting for Redis at all.
func TestCache_BreakerSkipsHangingRedis(t *testing.T) {
	env := newTestEnv(t)
	addr := env.redis.Addr()
	env.redis.Close()
	listener, err := net.Listen("tcp", addr) // accepts connections, never answers
	if err != nil {
		t.Skipf("cannot listen on the Redis address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	for range 3 { // each waits for the 100ms Redis timeout and opens the breaker
		env.get(t)
	}
	start := time.Now()
	env.get(t)
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("request with an open breaker took %v, want no Redis wait", elapsed)
	}
}

func TestCache_NotFoundIsNotCached(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.cache.GetByID(context.Background(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if keys := env.redis.Keys(); len(keys) != 0 {
		t.Errorf("cache keys = %v, want none", keys)
	}
}

func TestCache_RedisDownFallsBackToDatabase(t *testing.T) {
	env := newTestEnv(t)
	addr := env.redis.Addr()
	env.redis.Close()
	// A listener that never answers: a hanging Redis must not slow requests down.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on the Redis address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	start := time.Now()
	if got := env.get(t); got.ID != env.order.ID {
		t.Fatalf("got order %s", got.ID)
	}
	if _, err := env.cache.UpdateStatus(context.Background(), env.order.ID, nil, domain.StatusPaid, time.Now()); err != nil {
		t.Fatalf("update must succeed without Redis: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v with a hanging Redis, want well under 1s", elapsed)
	}
}

func TestSetIfNewer(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	run := func(version, payload string) int64 {
		n, err := setIfNewer.Run(context.Background(), rdb, []string{"k"}, version, payload, 60000).Int64()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	steps := []struct {
		version, payload string
		want             int64
		desc             string
	}{
		{"10", "v10", 1, "empty key"},
		{"9", "v9", 0, "older data"},
		{"10", "v10", 0, "same data again"},
		{"11", "", 1, "tombstone of a newer version"},
		{"10", "v10", 0, "older data over the tombstone"},
		{"11", "", 0, "same tombstone again"},
		{"11", "v11", 1, "data of the tombstone's version"},
		{"12", "v12", 1, "newer data"},
	}
	for _, step := range steps {
		if got := run(step.version, step.payload); got != step.want {
			t.Errorf("%s: set(%s, %q) = %d, want %d", step.desc, step.version, step.payload, got, step.want)
		}
	}
	if got, _ := mr.Get("k"); got != "12|v12" {
		t.Errorf("final value = %q, want 12|v12", got)
	}
}
