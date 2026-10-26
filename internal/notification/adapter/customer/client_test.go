package customer

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	"github.com/elita001/order-microservices/internal/notification/application"
	"github.com/elita001/order-microservices/pkg/breaker"
	"github.com/elita001/order-microservices/pkg/grpcx"
)

type fakeCustomers struct {
	customerv1.UnimplementedCustomerServiceServer
	email string
	err   error
}

func (f *fakeCustomers) GetCustomer(_ context.Context, req *customerv1.GetCustomerRequest) (*customerv1.GetCustomerResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &customerv1.GetCustomerResponse{Customer: &customerv1.Customer{Id: req.GetCustomerId(), Email: f.email}}, nil
}

func newClient(t *testing.T, fake *fakeCustomers) *Client {
	t.Helper()
	srv := grpc.NewServer()
	customerv1.RegisterCustomerServiceServer(srv, fake)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpcx.NewClient(grpcx.ClientConfig{Addr: lis.Addr().String(), Timeout: time.Second},
		grpcx.ClientOptions{Service: customerv1.CustomerService_ServiceDesc.ServiceName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewClient(conn, breaker.Config{Failures: 2, OpenTimeout: time.Minute, HalfOpenRequests: 1}, slog.New(slog.DiscardHandler))
}

func TestEmail(t *testing.T) {
	c := newClient(t, &fakeCustomers{email: "alice@example.com"})
	email, err := c.Email(context.Background(), uuid.New())
	if err != nil || email != "alice@example.com" {
		t.Fatalf("got %q, %v", email, err)
	}
}

// What the consumer does with the message depends on the error: permanent errors go to the
// DLQ at once, the others are retried with backoff.
func TestEmail_ErrorClassification(t *testing.T) {
	tests := map[string]struct {
		fake      *fakeCustomers
		permanent bool
	}{
		"unknown customer": {fake: &fakeCustomers{err: status.Error(codes.NotFound, "customer not found")}, permanent: true},
		"no email":         {fake: &fakeCustomers{email: ""}, permanent: true},
		"service down":     {fake: &fakeCustomers{err: status.Error(codes.Unavailable, "down")}, permanent: false},
		"rejected token":   {fake: &fakeCustomers{err: status.Error(codes.Unauthenticated, "bad token")}, permanent: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := newClient(t, tt.fake).Email(context.Background(), uuid.New())
			if err == nil {
				t.Fatal("expected an error")
			}
			if application.IsPermanentError(err) != tt.permanent {
				t.Errorf("IsPermanentError(%v) = %v, want %v", err, !tt.permanent, tt.permanent)
			}
		})
	}
}

func TestEmail_BreakerOpensOnlyForOutages(t *testing.T) {
	notFound := newClient(t, &fakeCustomers{err: status.Error(codes.NotFound, "customer not found")})
	for range 3 {
		if _, err := notFound.Email(context.Background(), uuid.New()); errors.Is(err, breaker.ErrOpen) {
			t.Fatal("unknown customers opened the breaker")
		}
	}

	down := newClient(t, &fakeCustomers{err: status.Error(codes.Unavailable, "down")})
	for range 2 {
		_, _ = down.Email(context.Background(), uuid.New())
	}
	if _, err := down.Email(context.Background(), uuid.New()); !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("got %v, want ErrOpen after 2 failures", err)
	}
}
