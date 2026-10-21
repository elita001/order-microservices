package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/elita001/order-microservices/internal/gateway/idempotency"
	"github.com/elita001/order-microservices/internal/gateway/proxy"
	"github.com/elita001/order-microservices/internal/gateway/ratelimit"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/redisclient"
)

type testEnv struct {
	gateway   *httptest.Server
	redis     *miniredis.Miniredis
	calls     atomic.Int32
	lastReqID atomic.Value
}

// newTestEnv starts the gateway in front of a fake order service. The upstream answers
// 201 to POST, or the status from the X-Test-Status header, after X-Test-Delay.
func newTestEnv(t *testing.T, writeLimit ratelimit.Policy) *testEnv {
	t.Helper()
	env := &testEnv{redis: miniredis.RunT(t)}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.calls.Add(1)
		env.lastReqID.Store(r.Header.Get(httpx.RequestIDHeader))
		if delay, err := time.ParseDuration(r.Header.Get("X-Test-Delay")); err == nil {
			time.Sleep(delay)
		}
		status := http.StatusCreated
		if r.Header.Get("X-Test-Status") == "500" {
			status = http.StatusInternalServerError
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"call":%d,"echo":%s}`, env.calls.Load(), body)
	}))
	t.Cleanup(upstream.Close)

	target, _ := url.Parse(upstream.URL)
	rdb := newRedisClient(t, env.redis.Addr())

	router := NewRouter(Config{
		OrderServiceURL: target,
		Upstream:        proxy.Config{DialTimeout: time.Second, Timeout: 300 * time.Millisecond},
		ReadLimit:       ratelimit.Policy{Name: "read", Rate: 100, Burst: 100},
		WriteLimit:      writeLimit,
		Idempotency:     idempotency.Config{TTL: time.Hour, LockTTL: 10 * time.Second},
		RedisTimeout:    100 * time.Millisecond,
	}, rdb, slog.New(slog.DiscardHandler))
	env.gateway = httptest.NewServer(router)
	t.Cleanup(env.gateway.Close)
	return env
}

// newRedisClient uses the production client settings. The client's own timeouts are long
// on purpose: requests must be bounded by the gateway's RedisTimeout (via context) alone.
func newRedisClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	rdb, err := redisclient.New(redisclient.Config{
		URL:          "redis://" + addr,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("create redis client: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

var generousLimit = ratelimit.Policy{Name: "write", Rate: 100, Burst: 100}

func (env *testEnv) post(t *testing.T, key, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, env.gateway.URL+"/api/v1/orders", strings.NewReader(body))
	if key != "" {
		req.Header.Set(httpx.IdempotencyKeyHeader, key)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestIdempotency_RepeatIsReplayed(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	first, firstBody := env.post(t, "key-1", `{"a":1}`, nil)
	second, secondBody := env.post(t, "key-1", `{"a":1}`, nil)

	if first.StatusCode != http.StatusCreated || second.StatusCode != http.StatusCreated {
		t.Fatalf("statuses = %d, %d, want 201, 201", first.StatusCode, second.StatusCode)
	}
	if firstBody != secondBody {
		t.Errorf("replayed body %q differs from %q", secondBody, firstBody)
	}
	if second.Header.Get(httpx.IdempotentReplayedHeader) != "true" || first.Header.Get(httpx.IdempotentReplayedHeader) != "" {
		t.Error("only the repeat must have the Idempotent-Replayed header")
	}
	if second.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", second.Header.Get("Content-Type"))
	}
	if calls := env.calls.Load(); calls != 1 {
		t.Errorf("upstream called %d times, want 1", calls)
	}
}

func TestIdempotency_DifferentBodyIsRejected(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.post(t, "key-1", `{"a":1}`, nil)

	resp, body := env.post(t, "key-1", `{"a":2}`, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "idempotency_key_reused") {
		t.Fatalf("got %d %s, want 422 idempotency_key_reused", resp.StatusCode, body)
	}
}

func TestIdempotency_ConcurrentRepeatGetsConflict(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	var wg sync.WaitGroup
	wg.Go(func() { env.post(t, "key-1", `{}`, map[string]string{"X-Test-Delay": "200ms"}) })
	time.Sleep(50 * time.Millisecond)
	resp, _ := env.post(t, "key-1", `{}`, nil)
	wg.Wait()

	if resp.StatusCode != http.StatusConflict || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d, want 409 with Retry-After", resp.StatusCode)
	}
	if calls := env.calls.Load(); calls != 1 {
		t.Errorf("upstream called %d times, want 1", calls)
	}
}

func TestIdempotency_ServerErrorIsNotStored(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	first, _ := env.post(t, "key-1", `{}`, map[string]string{"X-Test-Status": "500"})
	second, _ := env.post(t, "key-1", `{}`, nil)

	if first.StatusCode != http.StatusInternalServerError || second.StatusCode != http.StatusCreated {
		t.Fatalf("statuses = %d, %d, want 500, 201", first.StatusCode, second.StatusCode)
	}
	if calls := env.calls.Load(); calls != 2 {
		t.Errorf("upstream called %d times, want 2: a failed request must be retryable", calls)
	}
}

func TestIdempotency_RedisDownFailsOpen(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.redis.Close()

	resp, _ := env.post(t, "key-1", `{}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d, want 201: the gateway must work without Redis", resp.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	env := newTestEnv(t, ratelimit.Policy{Name: "write", Rate: 1, Burst: 2})

	var statuses []int
	for range 3 {
		resp, _ := env.post(t, "", `{}`, nil)
		statuses = append(statuses, resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	}
	if statuses[0] != http.StatusCreated || statuses[1] != http.StatusCreated || statuses[2] != http.StatusTooManyRequests {
		t.Errorf("statuses = %v, want [201 201 429]", statuses)
	}
}

func TestProxy_UpstreamTimeout(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	resp, body := env.post(t, "", `{}`, map[string]string{"X-Test-Delay": "1s"})
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(body, "upstream_timeout") {
		t.Fatalf("got %d %s, want 504 upstream_timeout", resp.StatusCode, body)
	}
}

func TestProxy_UpstreamUnavailable(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1") // nothing listens on port 1
	router := NewRouter(Config{
		OrderServiceURL: target,
		Upstream:        proxy.Config{DialTimeout: time.Second, Timeout: time.Second},
		ReadLimit:       ratelimit.Policy{Name: "read", Rate: 100, Burst: 100},
		WriteLimit:      generousLimit,
		RedisTimeout:    100 * time.Millisecond,
	}, newRedisClient(t, miniredis.RunT(t).Addr()), slog.New(slog.DiscardHandler))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/orders/123", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
}

func TestRequestIDIsForwarded(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	resp, _ := env.post(t, "", `{}`, map[string]string{httpx.RequestIDHeader: "trace-42"})
	if got := env.lastReqID.Load(); got != "trace-42" {
		t.Errorf("upstream got request ID %v, want trace-42", got)
	}
	if got := resp.Header.Get(httpx.RequestIDHeader); got != "trace-42" {
		t.Errorf("response request ID = %q, want trace-42", got)
	}
}

// A hanging Redis must not delay requests by more than the Redis timeout.
func TestRedisHangingFailsOpenQuickly(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	addr := env.redis.Addr()
	env.redis.Close()
	listener, err := net.Listen("tcp", addr) // accepts connections, never answers
	if err != nil {
		t.Skipf("cannot listen on the Redis address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	start := time.Now()
	resp, _ := env.post(t, "key-1", `{}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d, want 201", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("request took %v with a hanging Redis, want well under 1s", elapsed)
	}
}
