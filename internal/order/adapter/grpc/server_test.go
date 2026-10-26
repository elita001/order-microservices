package grpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/elita001/order-microservices/gen/go/orderflow/order/v1"
	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/grpcx"
)

type fakeService struct {
	createIn application.CreateOrderInput
	listIn   application.ListOrdersInput
	viewer   application.Viewer
	err      error
}

var order = &domain.Order{
	ID: uuid.New(), CustomerID: uuid.New(), Status: domain.StatusPaid, Currency: "RUB", TotalAmount: 300,
	Items:     []domain.Item{{ProductID: "p1", Quantity: 3, Price: 100}},
	CreatedAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 23, 12, 5, 0, 0, time.UTC),
}

func (f *fakeService) Create(_ context.Context, in application.CreateOrderInput) (*application.CreateOrderResult, error) {
	f.createIn = in
	if f.err != nil {
		return nil, f.err
	}
	return &application.CreateOrderResult{Order: order, Replayed: true}, nil
}

func (f *fakeService) Get(_ context.Context, viewer application.Viewer, _ uuid.UUID) (*domain.Order, error) {
	f.viewer = viewer
	return order, f.err
}

func (f *fakeService) ListByCustomer(_ context.Context, in application.ListOrdersInput) (*application.ListOrdersResult, error) {
	f.listIn = in
	return &application.ListOrdersResult{Orders: []domain.Order{*order}, Limit: 20}, f.err
}

func asUser(id uuid.UUID, roles ...string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{UserID: id, Roles: roles})
}

func TestCreateOrder_CustomerComesFromToken(t *testing.T) {
	svc := &fakeService{}
	userID := uuid.New()

	resp, err := NewServer(svc).CreateOrder(asUser(userID), &orderv1.CreateOrderRequest{
		Currency: "RUB", IdempotencyKey: "k1", Items: []*orderv1.OrderItem{{ProductId: "p1", Quantity: 3, Price: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if svc.createIn.CustomerID != userID || svc.createIn.IdempotencyKey != "k1" || svc.createIn.Items[0].Quantity != 3 {
		t.Errorf("input = %+v", svc.createIn)
	}
	got := resp.GetOrder()
	if !resp.GetReplayed() || got.GetStatus() != orderv1.OrderStatus_ORDER_STATUS_PAID || got.GetTotalAmount() != 300 ||
		!got.GetUpdateTime().AsTime().Equal(order.UpdatedAt) || got.GetItems()[0].GetPrice() != 100 {
		t.Errorf("response = %v", resp)
	}
}

func TestViewerRoles(t *testing.T) {
	svc := &fakeService{}
	server := NewServer(svc)

	if _, err := server.GetOrder(asUser(uuid.New(), auth.RoleCustomer), &orderv1.GetOrderRequest{OrderId: order.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if svc.viewer.SeeAll {
		t.Error("a customer must not see all orders")
	}
	if _, err := server.ListOrders(asUser(uuid.New(), auth.RoleSupport), &orderv1.ListOrdersRequest{CustomerId: order.CustomerID.String()}); err != nil {
		t.Fatal(err)
	}
	if !svc.listIn.Viewer.SeeAll || svc.listIn.CustomerID != order.CustomerID {
		t.Errorf("list input = %+v", svc.listIn)
	}
}

func TestErrorsToStatus(t *testing.T) {
	tests := map[string]struct {
		err    error
		code   codes.Code
		reason string
	}{
		"validation":     {&domain.ValidationError{Field: "currency", Message: "must be one of RUB, USD, EUR"}, codes.InvalidArgument, ReasonValidationFailed},
		"key reused":     {domain.ErrIdempotencyKeyReused, codes.InvalidArgument, ReasonIdempotencyKeyReused},
		"not found":      {domain.ErrNotFound, codes.NotFound, ReasonOrderNotFound},
		"forbidden":      {domain.ErrForbidden, codes.PermissionDenied, ReasonForbidden},
		"database down":  {context.DeadlineExceeded, codes.Unavailable, ReasonDatabaseUnavailable},
		"unexpected bug": {errors.New("boom"), codes.Internal, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewServer(&fakeService{err: tt.err}).CreateOrder(asUser(uuid.New()), &orderv1.CreateOrderRequest{})
			if status.Code(err) != tt.code || grpcx.Reason(err) != tt.reason {
				t.Errorf("got %v (reason %q), want %v %q", err, grpcx.Reason(err), tt.code, tt.reason)
			}
		})
	}
	field, _, ok := grpcx.FieldViolation(toStatus(context.Background(), &domain.ValidationError{Field: "currency", Message: "bad"}))
	if !ok || field != "currency" {
		t.Errorf("field violation = %q, %v", field, ok)
	}
}

func TestInvalidIDs(t *testing.T) {
	server := NewServer(&fakeService{})
	if _, err := server.GetOrder(asUser(uuid.New()), &orderv1.GetOrderRequest{OrderId: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetOrder: %v", err)
	}
	if _, err := server.ListOrders(asUser(uuid.New()), &orderv1.ListOrdersRequest{CustomerId: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListOrders: %v", err)
	}
}
