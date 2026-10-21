// Package cache implements a cache-aside decorator for the order repository.
//
// Reads of a single order are served from Redis and fall back to the wrapped repository
// on a miss. Status changes invalidate the entry. Redis is optional: on any Redis error
// the decorator logs it and works with the database directly (fail-open).
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/pkg/breaker"
	"github.com/elita001/order-microservices/pkg/logger"
)

// Config is usually loaded from environment variables with the CACHE_ prefix.
type Config struct {
	// TTL bounds staleness if an invalidation is lost (e.g., Redis was unavailable
	// at that moment). A random jitter of up to 10% is added, so entries cached at
	// the same time do not expire at the same time.
	TTL time.Duration `env:"TTL" envDefault:"5m"`
	// LoadTimeout bounds a database load shared by concurrent misses (see GetByID).
	LoadTimeout time.Duration `env:"LOAD_TIMEOUT" envDefault:"5s"`
}

// keyPrefix includes a format version: changing cachedOrder requires a new version,
// so old entries are ignored instead of failing to decode.
const keyPrefix = "order:v1:"

// setIfNewer stores "<version>|<payload>" unless the key already holds a newer version.
// The version is updated_at in microseconds. An empty payload is a tombstone written on
// invalidation: it blocks older data, but data of the same version may replace it.
//
// This closes the classic cache-aside race: a reader loads the order from the database,
// the status changes and the entry is invalidated, and only then the reader writes the
// stale order to the cache. Here the stale write is rejected because its version is older.
var setIfNewer = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current then
	local sep = string.find(current, '|', 1, true)
	local currentVersion = tonumber(string.sub(current, 1, sep - 1))
	local newVersion = tonumber(ARGV[1])
	local currentHasData = #current > sep
	if currentVersion > newVersion or (currentVersion == newVersion and (currentHasData or ARGV[2] == '')) then
		return 0
	end
end
redis.call('SET', KEYS[1], ARGV[1] .. '|' .. ARGV[2], 'PX', ARGV[3])
return 1
`)

// Repository decorates application.Repository; methods other than GetByID and
// UpdateStatus go straight to the wrapped repository.
type Repository struct {
	application.Repository

	rdb     *redis.Client
	cfg     Config
	timeout time.Duration // per Redis call
	loads   singleflight.Group
	log     *slog.Logger
}

func New(inner application.Repository, rdb *redis.Client, cfg Config, timeout time.Duration, log *slog.Logger) *Repository {
	return &Repository{
		Repository: inner,
		rdb:        rdb,
		cfg:        cfg,
		timeout:    timeout,
		log:        log.With("component", "order_cache"),
	}
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Order, error) {
	if order, ok := r.get(ctx, id); ok {
		return order, nil
	}

	// Concurrent misses for the same order share one database query (no stampede
	// when a popular entry expires). The shared load is detached from the caller that
	// started it: if that client disconnects, the others still get the order. Each caller
	// waits only as long as its own context allows.
	loaded := r.loads.DoChan(id.String(), func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.LoadTimeout)
		defer cancel()
		order, err := r.Repository.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		r.set(ctx, order)
		return order, nil
	})
	select {
	case res := <-loaded:
		if res.Err != nil {
			return nil, res.Err
		}
		order := *res.Val.(*domain.Order) // callers must not share one struct
		return &order, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Repository) UpdateStatus(ctx context.Context, id uuid.UUID, from []domain.Status, to domain.Status, updatedAt time.Time) (bool, error) {
	updated, err := r.Repository.UpdateStatus(ctx, id, from, to, updatedAt)
	if err != nil || !updated {
		return updated, err
	}
	r.invalidate(ctx, id, updatedAt)
	return true, nil
}

func (r *Repository) get(ctx context.Context, id uuid.UUID) (*domain.Order, bool) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	raw, err := r.rdb.Get(ctx, keyPrefix+id.String()).Result()
	if err == redis.Nil {
		return nil, false
	}
	if err != nil {
		r.warn(ctx, "read order from cache", err)
		return nil, false
	}
	_, payload, _ := strings.Cut(raw, "|")
	if payload == "" { // tombstone: recently invalidated
		return nil, false
	}
	var cached cachedOrder
	if err := json.Unmarshal([]byte(payload), &cached); err != nil {
		r.warn(ctx, "decode cached order", err)
		return nil, false
	}
	logger.FromContext(ctx).Debug("order cache hit", "order_id", id)
	return cached.toDomain(), true
}

func (r *Repository) set(ctx context.Context, order *domain.Order) {
	payload, err := json.Marshal(fromDomain(order))
	if err != nil {
		r.warn(ctx, "encode order for cache", err)
		return
	}
	r.store(ctx, order.ID, order.UpdatedAt, string(payload))
}

// invalidate writes a tombstone with the new version instead of deleting the key,
// so a concurrent reader cannot put the previous version back.
func (r *Repository) invalidate(ctx context.Context, id uuid.UUID, updatedAt time.Time) {
	r.store(ctx, id, updatedAt, "")
}

func (r *Repository) store(ctx context.Context, id uuid.UUID, version time.Time, payload string) {
	// Detached from the request: a client disconnect must not skip an invalidation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
	defer cancel()

	ttl := r.cfg.TTL + time.Duration(rand.Int64N(int64(r.cfg.TTL)/10+1))
	err := setIfNewer.Run(ctx, r.rdb, []string{keyPrefix + id.String()},
		strconv.FormatInt(version.UnixMicro(), 10), payload, ttl.Milliseconds()).Err()
	if err != nil {
		r.warn(ctx, fmt.Sprintf("write order %s to cache", id), err)
	}
}

func (r *Repository) warn(ctx context.Context, msg string, err error) {
	if errors.Is(err, breaker.ErrOpen) {
		return // an open breaker is logged once by itself
	}
	logger.FromContext(ctx).Warn(msg+" failed, using the database", "component", "order_cache", "error", err)
}

// cachedOrder is the cache format, decoupled from the domain struct on purpose.
type cachedOrder struct {
	ID             uuid.UUID    `json:"id"`
	CustomerID     uuid.UUID    `json:"customer_id"`
	Status         string       `json:"status"`
	Items          []cachedItem `json:"items"`
	TotalAmount    int64        `json:"total_amount"`
	Currency       string       `json:"currency"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	RequestHash    string       `json:"request_hash,omitempty"`
}

type cachedItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
	Price     int64  `json:"price"`
}

func fromDomain(o *domain.Order) cachedOrder {
	items := make([]cachedItem, 0, len(o.Items))
	for _, item := range o.Items {
		items = append(items, cachedItem{ProductID: item.ProductID, Quantity: item.Quantity, Price: item.Price})
	}
	return cachedOrder{
		ID: o.ID, CustomerID: o.CustomerID, Status: string(o.Status), Items: items,
		TotalAmount: o.TotalAmount, Currency: o.Currency, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
		IdempotencyKey: o.IdempotencyKey, RequestHash: o.RequestHash,
	}
}

func (c cachedOrder) toDomain() *domain.Order {
	items := make([]domain.Item, 0, len(c.Items))
	for _, item := range c.Items {
		items = append(items, domain.Item{ProductID: item.ProductID, Quantity: item.Quantity, Price: item.Price})
	}
	return &domain.Order{
		ID: c.ID, CustomerID: c.CustomerID, Status: domain.Status(c.Status), Items: items,
		TotalAmount: c.TotalAmount, Currency: c.Currency, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		IdempotencyKey: c.IdempotencyKey, RequestHash: c.RequestHash,
	}
}
