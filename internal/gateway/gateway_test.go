package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	orderv1 "github.com/elita001/order-microservices/gen/go/orderflow/order/v1"
	"github.com/elita001/order-microservices/internal/gateway/idempotency"
	"github.com/elita001/order-microservices/internal/gateway/ratelimit"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/auth/authtest"
	"github.com/elita001/order-microservices/pkg/breaker"
	"github.com/elita001/order-microservices/pkg/grpcx"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/redisclient"
)

const (
	orderBody      = `{"currency":"RUB","items":[{"product_id":"p1","quantity":1,"price":100}]}`
	otherOrderBody = `{"currency":"RUB","items":[{"product_id":"p1","quantity":2,"price":100}]}`
)

// fakeService records the calls of the gateway and answers like the order and customer
// services. The next call can be delayed or failed.
type fakeService struct {
	orderv1.UnimplementedOrderServiceServer
	customerv1.UnimplementedCustomerServiceServer

	calls atomic.Int32

	mu        sync.Mutex
	lastMD    metadata.MD
	lastReq   any
	delayNext time.Duration
	failNext  error
}

func (f *fakeService) begin(ctx context.Context, req any) error {
	f.calls.Add(1)
	f.mu.Lock()
	md, _ := metadata.FromIncomingContext(ctx)
	f.lastMD, f.lastReq = md, req
	delay, fail := f.delayNext, f.failNext
	f.delayNext, f.failNext = 0, nil
	f.mu.Unlock()

	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
	return fail
}

func (f *fakeService) next(delay time.Duration, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delayNext, f.failNext = delay, err
}

func (f *fakeService) metadata(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if values := f.lastMD.Get(key); len(values) > 0 {
		return values[0]
	}
	return ""
}

var created = timestamppb.New(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))

func (f *fakeService) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) {
	if err := f.begin(ctx, req); err != nil {
		return nil, err
	}
	// A new ID on every call, so a replay from the gateway's cache is visible.
	return &orderv1.CreateOrderResponse{Order: &orderv1.Order{
		Id: uuid.NewString(), Status: orderv1.OrderStatus_ORDER_STATUS_PENDING, Currency: req.GetCurrency(),
		Items: req.GetItems(), CreateTime: created, UpdateTime: created,
	}}, nil
}

func (f *fakeService) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	if err := f.begin(ctx, req); err != nil {
		return nil, err
	}
	return &orderv1.GetOrderResponse{Order: &orderv1.Order{
		Id: req.GetOrderId(), CustomerId: "c1", Status: orderv1.OrderStatus_ORDER_STATUS_PAYMENT_FAILED,
		Items:       []*orderv1.OrderItem{{ProductId: "p1", Quantity: 3, Price: 250}},
		TotalAmount: 750, Currency: "EUR", CreateTime: created, UpdateTime: created,
	}}, nil
}

func (f *fakeService) ListOrders(ctx context.Context, req *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	if err := f.begin(ctx, req); err != nil {
		return nil, err
	}
	return &orderv1.ListOrdersResponse{Limit: 20, Orders: []*orderv1.OrderSummary{
		{Id: "o1", Status: orderv1.OrderStatus_ORDER_STATUS_SHIPPED, TotalAmount: 100, Currency: "RUB", CreateTime: created},
	}}, nil
}

func (f *fakeService) GetCustomer(ctx context.Context, req *customerv1.GetCustomerRequest) (*customerv1.GetCustomerResponse, error) {
	if err := f.begin(ctx, req); err != nil {
		return nil, err
	}
	return &customerv1.GetCustomerResponse{Customer: &customerv1.Customer{
		Id: req.GetCustomerId(), Username: "alice", Email: "alice@example.com", SyncTime: created,
	}}, nil
}

type testEnv struct {
	idp     *authtest.IDP
	userID  uuid.UUID
	token   string // a valid token of the default test user
	gateway *httptest.Server
	redis   *miniredis.Miniredis
	fake    *fakeService
}

// newTestEnv starts the gateway in front of fake order and customer services (gRPC).
func newTestEnv(t *testing.T, writeLimit ratelimit.Policy) *testEnv {
	t.Helper()
	env := &testEnv{redis: miniredis.RunT(t), idp: authtest.NewIDP(t), fake: &fakeService{}, userID: uuid.New()}
	env.token = env.idp.Customer(t, env.userID)

	srv := grpc.NewServer()
	orderv1.RegisterOrderServiceServer(srv, env.fake)
	customerv1.RegisterCustomerServiceServer(srv, env.fake)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	env.gateway = httptest.NewServer(newRouter(t, lis.Addr().String(), env.idp, newRedisClient(t, env.redis.Addr()), writeLimit))
	t.Cleanup(env.gateway.Close)
	return env
}

func newRouter(t *testing.T, addr string, idp *authtest.IDP, rdb *redis.Client, writeLimit ratelimit.Policy) http.Handler {
	t.Helper()
	cfg := grpcx.ClientConfig{Addr: addr, Timeout: 300 * time.Millisecond}
	orderConn, err := grpcx.NewClient(cfg, grpcx.ClientOptions{
		Service: orderv1.OrderService_ServiceDesc.ServiceName, RetryMethods: []string{"GetOrder"}, Credentials: grpcx.ForwardToken(),
	})
	if err != nil {
		t.Fatal(err)
	}
	customerConn, err := grpcx.NewClient(cfg, grpcx.ClientOptions{
		Service: customerv1.CustomerService_ServiceDesc.ServiceName, Credentials: grpcx.ForwardToken(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orderConn.Close(); _ = customerConn.Close() })

	return NewRouter(Config{
		Orders:       orderv1.NewOrderServiceClient(orderConn),
		Customers:    customerv1.NewCustomerServiceClient(customerConn),
		ReadLimit:    ratelimit.Policy{Name: "read", Rate: 100, Burst: 100},
		WriteLimit:   writeLimit,
		Idempotency:  idempotency.Config{TTL: time.Hour, LockTTL: 10 * time.Second},
		RedisTimeout: 100 * time.Millisecond,
		Verifier:     auth.NewVerifier(context.Background(), idp.Config),
	}, rdb, slog.New(slog.DiscardHandler))
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
		Breaker:      breaker.Config{Failures: 5, OpenTimeout: time.Minute, HalfOpenRequests: 1},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("create redis client: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

var generousLimit = ratelimit.Policy{Name: "write", Rate: 100, Burst: 100}

func (env *testEnv) do(t *testing.T, method, path, key, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, env.gateway.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set(httpx.IdempotencyKeyHeader, key)
	}
	req.Header.Set("Authorization", "Bearer "+env.token) // headers may override or clear it
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

func (env *testEnv) post(t *testing.T, key, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	return env.do(t, http.MethodPost, "/api/v1/orders", key, body, headers)
}

func (env *testEnv) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	return env.do(t, http.MethodGet, path, "", "", nil)
}

func TestCreateOrder_TranslatesToGRPC(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	resp, body := env.post(t, "key-1", orderBody, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d %s, want 201", resp.StatusCode, body)
	}
	req := env.fake.lastReq.(*orderv1.CreateOrderRequest)
	if req.GetIdempotencyKey() != "key-1" || req.GetCurrency() != "RUB" || req.GetItems()[0].GetPrice() != 100 {
		t.Errorf("gRPC request = %v", req)
	}
	var order orderResponse
	if err := json.Unmarshal([]byte(body), &order); err != nil || order.Status != "pending" || len(order.Items) != 1 {
		t.Errorf("response = %s (%v)", body, err)
	}
}

func TestCreateOrder_CustomerIDInBodyIsRejected(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	resp, _ := env.post(t, "", `{"customer_id":"`+uuid.NewString()+`","currency":"RUB","items":[]}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
	if calls := env.fake.calls.Load(); calls != 0 {
		t.Errorf("order service called %d times", calls)
	}
}

func TestGetOrder_ResponseFormat(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	resp, body := env.get(t, "/api/v1/orders/o-42")
	want := `{"id":"o-42","customer_id":"c1","status":"payment_failed","total_amount":750,"currency":"EUR",` +
		`"items":[{"product_id":"p1","quantity":3,"price":250}],` +
		`"created_at":"2026-10-23T12:00:00Z","updated_at":"2026-10-23T12:00:00Z"}`
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != want {
		t.Fatalf("got %d\n%s\nwant\n%s", resp.StatusCode, body, want)
	}
}

func TestListOrders(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	resp, body := env.get(t, "/api/v1/orders?customer_id=c9&limit=5&offset=10")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"status":"shipped"`) {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	req := env.fake.lastReq.(*orderv1.ListOrdersRequest)
	if req.GetCustomerId() != "c9" || req.GetLimit() != 5 || req.GetOffset() != 10 {
		t.Errorf("gRPC request = %v", req)
	}

	resp, body = env.get(t, "/api/v1/orders?limit=lots")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "validation_error") {
		t.Errorf("invalid limit: got %d %s, want 422", resp.StatusCode, body)
	}
}

func TestCustomersMe_AsksForTheAuthenticatedUser(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	resp, body := env.get(t, "/api/v1/customers/me")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"email":"alice@example.com"`) {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if id := env.fake.lastReq.(*customerv1.GetCustomerRequest).GetCustomerId(); id != env.userID.String() {
		t.Errorf("asked for customer %s, want the token's subject %s", id, env.userID)
	}
}

// Zero trust: the service gets the user's own token and authenticates the user itself.
func TestUserTokenAndRequestIDAreForwarded(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	resp, _ := env.post(t, "", orderBody, map[string]string{httpx.RequestIDHeader: "trace-42"})
	if got := env.fake.metadata("authorization"); got != "Bearer "+env.token {
		t.Errorf("authorization metadata = %q, want the user's token", got)
	}
	// The gateway generates the request ID itself and forwards it; a client's ID is never used.
	generated := resp.Header.Get(httpx.RequestIDHeader)
	if generated == "" || generated == "trace-42" {
		t.Fatalf("response request ID = %q, want a generated one", generated)
	}
	if got := env.fake.metadata("x-request-id"); got != generated {
		t.Errorf("service got request ID %q, want the generated %s", got, generated)
	}
}

func TestErrorMapping(t *testing.T) {
	const domain = "order.orderflow"
	tests := map[string]struct {
		err        error
		status     int
		code       string
		retryAfter bool
	}{
		"validation": {
			err:    grpcx.FieldError(domain, "VALIDATION_FAILED", "order_id", "must be a valid UUID"),
			status: http.StatusUnprocessableEntity, code: "validation_error",
		},
		"reused idempotency key": {
			err:    grpcx.FieldError(domain, reasonIdempotencyKeyReused, "idempotency_key", "reused"),
			status: http.StatusUnprocessableEntity, code: "idempotency_key_reused",
		},
		"not found":         {err: status.Error(codes.NotFound, "order not found"), status: http.StatusNotFound, code: "not_found"},
		"permission denied": {err: status.Error(codes.PermissionDenied, "denied"), status: http.StatusForbidden, code: "forbidden"},
		"database down": {
			err:    grpcx.Error(codes.Unavailable, domain, "DATABASE_UNAVAILABLE", "retry later"),
			status: http.StatusServiceUnavailable, code: "service_unavailable", retryAfter: true,
		},
		"internal": {err: status.Error(codes.Internal, "boom"), status: http.StatusInternalServerError, code: "internal_error"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, generousLimit)
			env.fake.next(0, tt.err)

			// ListOrders: not retried in the test configuration, so the error reaches the client.
			resp, body := env.get(t, "/api/v1/orders")
			if resp.StatusCode != tt.status || !strings.Contains(body, `"code":"`+tt.code+`"`) {
				t.Fatalf("got %d %s, want %d %s", resp.StatusCode, body, tt.status, tt.code)
			}
			if tt.retryAfter != (resp.Header.Get("Retry-After") != "") {
				t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
			}
		})
	}
}

// GetOrder is configured as retryable: a transient UNAVAILABLE (like a broken connection)
// is retried by gRPC itself, and the client never sees it.
func TestReadsAreRetriedOnUnavailable(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.fake.next(0, status.Error(codes.Unavailable, "connection reset"))

	resp, body := env.get(t, "/api/v1/orders/o-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d %s, want 200 after a retry", resp.StatusCode, body)
	}
	if calls := env.fake.calls.Load(); calls != 2 {
		t.Errorf("order service called %d times, want 2", calls)
	}
}

func TestUpstreamTimeout(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.fake.next(time.Second, nil) // the client deadline is 300ms

	resp, body := env.post(t, "", orderBody, nil)
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(body, "upstream_timeout") {
		t.Fatalf("got %d %s, want 504 upstream_timeout", resp.StatusCode, body)
	}
}

func TestUpstreamUnavailable(t *testing.T) {
	idp := authtest.NewIDP(t)
	// Nothing listens on port 1.
	router := newRouter(t, "127.0.0.1:1", idp, newRedisClient(t, miniredis.RunT(t).Addr()), generousLimit)

	// ListOrders: not retried in the test configuration. Retries of GetOrder, with backoff,
	// would outlast the short test deadline and end as 504.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer "+idp.Customer(t, uuid.New()))
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_unavailable") {
		t.Fatalf("got %d %s, want 502 upstream_unavailable", rec.Code, rec.Body)
	}
}

func TestIdempotency_RepeatIsReplayed(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	first, firstBody := env.post(t, "key-1", orderBody, nil)
	second, secondBody := env.post(t, "key-1", orderBody, nil)

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
	if calls := env.fake.calls.Load(); calls != 1 {
		t.Errorf("order service called %d times, want 1", calls)
	}
}

func TestIdempotency_DifferentBodyIsRejected(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.post(t, "key-1", orderBody, nil)

	resp, body := env.post(t, "key-1", otherOrderBody, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "idempotency_key_reused") {
		t.Fatalf("got %d %s, want 422 idempotency_key_reused", resp.StatusCode, body)
	}
}

func TestIdempotency_ConcurrentRepeatGetsConflict(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.fake.next(200*time.Millisecond, nil)

	var wg sync.WaitGroup
	wg.Go(func() { env.post(t, "key-1", orderBody, nil) })
	time.Sleep(50 * time.Millisecond)
	resp, _ := env.post(t, "key-1", orderBody, nil)
	wg.Wait()

	if resp.StatusCode != http.StatusConflict || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d, want 409 with Retry-After", resp.StatusCode)
	}
	if calls := env.fake.calls.Load(); calls != 1 {
		t.Errorf("order service called %d times, want 1", calls)
	}
}

func TestIdempotency_ServerErrorIsNotStored(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.fake.next(0, status.Error(codes.Internal, "boom"))

	first, _ := env.post(t, "key-1", orderBody, nil)
	second, _ := env.post(t, "key-1", orderBody, nil)

	if first.StatusCode != http.StatusInternalServerError || second.StatusCode != http.StatusCreated {
		t.Fatalf("statuses = %d, %d, want 500, 201", first.StatusCode, second.StatusCode)
	}
	if calls := env.fake.calls.Load(); calls != 2 {
		t.Errorf("order service called %d times, want 2: a failed request must be retryable", calls)
	}
}

// A client that gives up leaves no response: the key must be released, not stored empty.
func TestIdempotency_AbandonedRequestIsNotStored(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.fake.next(200*time.Millisecond, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, env.gateway.URL+"/api/v1/orders", strings.NewReader(orderBody))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set(httpx.IdempotencyKeyHeader, "key-1")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the request was expected to time out on the client")
	}

	// The gateway notices the disconnect, cancels the call, and releases the key.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, body := env.post(t, "key-1", orderBody, nil)
		if resp.StatusCode == http.StatusCreated && resp.Header.Get(httpx.IdempotentReplayedHeader) == "" {
			return
		}
		if resp.StatusCode != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("got %d %s, want 201 created by the retry", resp.StatusCode, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestIdempotency_RedisDownFailsOpen(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	env.redis.Close()

	resp, _ := env.post(t, "key-1", orderBody, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d, want 201: the gateway must work without Redis", resp.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	env := newTestEnv(t, ratelimit.Policy{Name: "write", Rate: 1, Burst: 2})

	var statuses []int
	for range 3 {
		resp, _ := env.post(t, "", orderBody, nil)
		statuses = append(statuses, resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	}
	if statuses[0] != http.StatusCreated || statuses[1] != http.StatusCreated || statuses[2] != http.StatusTooManyRequests {
		t.Errorf("statuses = %v, want [201 201 429]", statuses)
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
	resp, _ := env.post(t, "key-1", orderBody, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d, want 201", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("request took %v with a hanging Redis, want well under 1s", elapsed)
	}
}

func TestAuth_RequestsWithoutValidTokenAreRejected(t *testing.T) {
	env := newTestEnv(t, generousLimit)

	for name, header := range map[string]string{
		"no token":      "",
		"invalid token": "Bearer not-a-jwt",
		"expired token": "Bearer " + env.idp.Sign(t, authtest.Token{Subject: uuid.New(), ExpiresIn: -time.Minute}),
	} {
		t.Run(name, func(t *testing.T) {
			resp, _ := env.post(t, "", orderBody, map[string]string{"Authorization": header})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("POST got %d, want 401", resp.StatusCode)
			}
			resp, _ = env.do(t, http.MethodGet, "/api/v1/customers/me", "", "", map[string]string{"Authorization": header})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("GET /customers/me got %d, want 401", resp.StatusCode)
			}
		})
	}
	if calls := env.fake.calls.Load(); calls != 0 {
		t.Errorf("services called %d times: unauthenticated requests must not reach them", calls)
	}
}

// Each user has their own budget: one user hitting the limit does not block another.
func TestRateLimit_PerUser(t *testing.T) {
	env := newTestEnv(t, ratelimit.Policy{Name: "write", Rate: 1, Burst: 1})
	other := map[string]string{"Authorization": "Bearer " + env.idp.Customer(t, uuid.New())}

	first, _ := env.post(t, "", orderBody, nil)
	limited, _ := env.post(t, "", orderBody, nil)
	otherUser, _ := env.post(t, "", orderBody, other)
	if first.StatusCode != http.StatusCreated || limited.StatusCode != http.StatusTooManyRequests || otherUser.StatusCode != http.StatusCreated {
		t.Errorf("statuses = %d, %d, %d; want 201, 429, 201", first.StatusCode, limited.StatusCode, otherUser.StatusCode)
	}
}

// Another user with the same Idempotency-Key and body must not get this user's response.
func TestIdempotency_KeysArePerUser(t *testing.T) {
	env := newTestEnv(t, generousLimit)
	other := map[string]string{"Authorization": "Bearer " + env.idp.Customer(t, uuid.New())}

	env.post(t, "shared-key", orderBody, nil)
	resp, _ := env.post(t, "shared-key", orderBody, other)
	if resp.Header.Get(httpx.IdempotentReplayedHeader) != "" {
		t.Fatal("another user got a replayed response")
	}
	if calls := env.fake.calls.Load(); calls != 2 {
		t.Errorf("order service called %d times, want 2", calls)
	}
}
