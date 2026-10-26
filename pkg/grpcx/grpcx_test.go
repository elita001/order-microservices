package grpcx

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/auth/authtest"
	"github.com/elita001/order-microservices/pkg/requestid"
)

// probe is a CustomerService whose behavior each test sets; it reports what the
// interceptors put into the handler's context.
type probe struct {
	customerv1.UnimplementedCustomerServiceServer
	handle func(ctx context.Context) error
}

func (p *probe) GetCustomer(ctx context.Context, _ *customerv1.GetCustomerRequest) (*customerv1.GetCustomerResponse, error) {
	if err := p.handle(ctx); err != nil {
		return nil, err
	}
	return &customerv1.GetCustomerResponse{}, nil
}

type testServer struct {
	idp      *authtest.IDP
	verifier *auth.Verifier
	probe    *probe
	client   customerv1.CustomerServiceClient
	health   healthpb.HealthClient
}

// start runs a server with the standard interceptors and returns a client that forwards
// the token of the calling context, like the gateway does.
func start(t *testing.T, timeout time.Duration) *testServer {
	t.Helper()
	idp := authtest.NewIDP(t)
	verifier := auth.NewVerifier(context.Background(), idp.Config)
	p := &probe{handle: func(context.Context) error { return nil }}

	srv := NewServer(ServerOptions{
		Logger:   slog.New(slog.DiscardHandler),
		Verifier: verifier,
		Timeout:  timeout,
	})
	customerv1.RegisterCustomerServiceServer(srv, p)
	h := RegisterHealth(srv, func(context.Context) error { return nil }, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := NewClient(ClientConfig{Addr: lis.Addr().String(), Timeout: time.Second}, ClientOptions{
		Service:     customerv1.CustomerService_ServiceDesc.ServiceName,
		Credentials: ForwardToken(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &testServer{idp: idp, verifier: verifier, probe: p, client: customerv1.NewCustomerServiceClient(conn), health: healthpb.NewHealthClient(conn)}
}

func call(ctx context.Context, s *testServer, opts ...grpc.CallOption) error {
	_, err := s.client.GetCustomer(ctx, &customerv1.GetCustomerRequest{}, opts...)
	return err
}

// userContext is what the gateway's auth middleware leaves in the request context.
func (s *testServer) userContext(t *testing.T, userID uuid.UUID) context.Context {
	t.Helper()
	ctx, err := s.verifier.Authenticate(context.Background(), s.idp.Customer(t, userID))
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// staticToken sends a fixed token, whatever the context holds.
type staticToken string

func (s staticToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(s)}, nil
}

func (staticToken) RequireTransportSecurity() bool { return false }

func TestAuth(t *testing.T) {
	s := start(t, 0)
	userID := uuid.New()

	if err := call(context.Background(), s); status.Code(err) != codes.Unauthenticated {
		t.Errorf("without a token: %v, want UNAUTHENTICATED", err)
	}
	expired := s.idp.Sign(t, authtest.Token{Subject: userID, ExpiresIn: -time.Minute})
	if err := call(context.Background(), s, grpc.PerRPCCredentials(staticToken(expired))); status.Code(err) != codes.Unauthenticated {
		t.Errorf("with an expired token: %v, want UNAUTHENTICATED", err)
	}

	var got auth.Principal
	s.probe.handle = func(ctx context.Context) error {
		got, _ = auth.FromContext(ctx)
		return nil
	}
	// The token the caller was authenticated with is forwarded (ForwardToken).
	if err := call(s.userContext(t, userID), s); err != nil {
		t.Fatal(err)
	}
	if got.UserID != userID || !got.HasRole(auth.RoleCustomer) {
		t.Errorf("principal = %+v, want user %s", got, userID)
	}
}

func TestHealthCheckNeedsNoToken(t *testing.T) {
	s := start(t, 0)
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := s.health.Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatalf("health check: %v", err)
		}
		if resp.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %v, want SERVING", resp.GetStatus())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRequestIDIsPropagated(t *testing.T) {
	s := start(t, 0)
	var got string
	s.probe.handle = func(ctx context.Context) error {
		got = requestid.FromContext(ctx)
		return nil
	}
	ctx := requestid.WithContext(s.userContext(t, uuid.New()), "req-42")
	if err := call(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got != "req-42" {
		t.Errorf("server request ID = %q, want req-42", got)
	}
}

func TestPanicBecomesInternal(t *testing.T) {
	s := start(t, 0)
	s.probe.handle = func(context.Context) error { panic("boom") }

	err := call(s.userContext(t, uuid.New()), s)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got %v, want INTERNAL", err)
	}
	// The server survived.
	s.probe.handle = func(context.Context) error { return nil }
	if err := call(s.userContext(t, uuid.New()), s); err != nil {
		t.Fatalf("after a panic: %v", err)
	}
}

func TestServerTimeoutCapsCallerDeadline(t *testing.T) {
	s := start(t, 50*time.Millisecond)
	var remaining time.Duration
	s.probe.handle = func(ctx context.Context) error {
		deadline, _ := ctx.Deadline()
		remaining = time.Until(deadline)
		return nil
	}
	// The client's default deadline is 1s; the server allows at most 50ms.
	if err := call(s.userContext(t, uuid.New()), s); err != nil {
		t.Fatal(err)
	}
	if remaining <= 0 || remaining > 50*time.Millisecond {
		t.Errorf("handler deadline in %v, want at most 50ms", remaining)
	}
}

func TestClientDefaultDeadline(t *testing.T) {
	s := start(t, 0)
	s.probe.handle = func(ctx context.Context) error {
		<-ctx.Done() // hangs until the caller gives up
		return ctx.Err()
	}
	begin := time.Now()
	err := call(s.userContext(t, uuid.New()), s)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("got %v, want DEADLINE_EXCEEDED", err)
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Errorf("the call took %v, want about the 1s default deadline", elapsed)
	}
}

// A transport failure is retried, but not a service's own "my dependency is down" answer.
func TestRetries(t *testing.T) {
	idp := authtest.NewIDP(t)
	calls := 0
	p := &probe{}
	srv := NewServer(ServerOptions{Logger: slog.New(slog.DiscardHandler), Verifier: auth.NewVerifier(context.Background(), idp.Config)})
	customerv1.RegisterCustomerServiceServer(srv, p)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := NewClient(ClientConfig{Addr: lis.Addr().String(), Timeout: 5 * time.Second}, ClientOptions{
		Service: customerv1.CustomerService_ServiceDesc.ServiceName, RetryMethods: []string{"GetCustomer"},
		Credentials: staticToken(idp.Customer(t, uuid.New())),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := customerv1.NewCustomerServiceClient(conn)

	for name, tt := range map[string]struct {
		err   error
		calls int
	}{
		"answered: dependency down":           {Error(codes.Unavailable, "customer.orderflow", "DIRECTORY_UNAVAILABLE", "retry later"), 1},
		"no reason: like a broken connection": {status.Error(codes.Unavailable, "connection reset"), 3},
	} {
		t.Run(name, func(t *testing.T) {
			calls = 0
			p.handle = func(context.Context) error { calls++; return tt.err }
			if _, err := client.GetCustomer(context.Background(), &customerv1.GetCustomerRequest{}); status.Code(err) != codes.Unavailable {
				t.Fatalf("got %v, want UNAVAILABLE", err)
			}
			if calls != tt.calls {
				t.Errorf("server called %d times, want %d", calls, tt.calls)
			}
		})
	}
}

func TestErrorDetails(t *testing.T) {
	err := FieldError("order.orderflow", "VALIDATION_FAILED", "items[0].quantity", "must be positive")
	if Reason(err) != "VALIDATION_FAILED" {
		t.Errorf("reason = %q", Reason(err))
	}
	field, description, ok := FieldViolation(err)
	if !ok || field != "items[0].quantity" || description != "must be positive" {
		t.Errorf("field violation = %q %q %v", field, description, ok)
	}
	if Reason(status.Error(codes.Unavailable, "no details")) != "" {
		t.Error("a status without ErrorInfo must have no reason")
	}
}

// The identity provider is down and the keys are not cached: UNAVAILABLE with a reason (the
// gateway answers 503), not UNAUTHENTICATED, which would make the user log in again.
func TestAuth_KeysUnavailable(t *testing.T) {
	idp := authtest.NewIDP(t)
	cfg := idp.Config
	cfg.JWKSURL = "http://127.0.0.1:1/certs" // nothing listens on port 1
	calls := 0
	srv := NewServer(ServerOptions{Logger: slog.New(slog.DiscardHandler), Verifier: auth.NewVerifier(context.Background(), cfg)})
	customerv1.RegisterCustomerServiceServer(srv, &probe{handle: func(context.Context) error { calls++; return nil }})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := NewClient(ClientConfig{Addr: lis.Addr().String(), Timeout: 5 * time.Second}, ClientOptions{
		Service: customerv1.CustomerService_ServiceDesc.ServiceName, RetryMethods: []string{"GetCustomer"},
		Credentials: staticToken(idp.Customer(t, uuid.New())),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_, err = customerv1.NewCustomerServiceClient(conn).GetCustomer(context.Background(), &customerv1.GetCustomerRequest{})
	if status.Code(err) != codes.Unavailable || Reason(err) != ReasonIdentityProviderUnavailable {
		t.Fatalf("got %v (reason %q), want UNAVAILABLE %s", err, Reason(err), ReasonIdentityProviderUnavailable)
	}
	if calls != 0 {
		t.Errorf("the handler ran %d times without an authenticated user", calls)
	}
}
