// Package grpc provides the gRPC delivery adapter for order use cases
// (orderflow.order.v1.OrderService).
package grpc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	orderv1 "github.com/elita001/order-microservices/gen/go/orderflow/order/v1"
	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/errtrack"
	"github.com/elita001/order-microservices/pkg/grpcx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/postgres"
)

// ErrorDomain is the google.rpc.ErrorInfo domain of this service's errors.
const ErrorDomain = "order.orderflow"

// Error reasons (google.rpc.ErrorInfo.reason): a stable part of the API contract.
const (
	ReasonValidationFailed     = "VALIDATION_FAILED"
	ReasonIdempotencyKeyReused = "IDEMPOTENCY_KEY_REUSED"
	ReasonOrderNotFound        = "ORDER_NOT_FOUND"
	ReasonForbidden            = "FORBIDDEN"
	ReasonDatabaseUnavailable  = "DATABASE_UNAVAILABLE"
)

type Service interface {
	Create(ctx context.Context, in application.CreateOrderInput) (*application.CreateOrderResult, error)
	Get(ctx context.Context, viewer application.Viewer, id uuid.UUID) (*domain.Order, error)
	ListByCustomer(ctx context.Context, in application.ListOrdersInput) (*application.ListOrdersResult, error)
}

type Server struct {
	orderv1.UnimplementedOrderServiceServer
	service Service
}

var _ orderv1.OrderServiceServer = (*Server)(nil)

func NewServer(service Service) *Server {
	return &Server{service: service}
}

func (s *Server) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) {
	items := make([]domain.Item, 0, len(req.GetItems()))
	for _, item := range req.GetItems() {
		items = append(items, domain.Item{ProductID: item.GetProductId(), Quantity: int(item.GetQuantity()), Price: item.GetPrice()})
	}
	result, err := s.service.Create(ctx, application.CreateOrderInput{
		CustomerID:     mustPrincipal(ctx).UserID,
		Currency:       req.GetCurrency(),
		Items:          items,
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, toStatus(ctx, err)
	}
	return &orderv1.CreateOrderResponse{Order: toProto(result.Order), Replayed: result.Replayed}, nil
}

func (s *Server) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	id, err := uuid.Parse(req.GetOrderId())
	if err != nil {
		return nil, toStatus(ctx, &domain.ValidationError{Field: "order_id", Message: "must be a valid UUID"})
	}
	order, err := s.service.Get(ctx, viewer(ctx), id)
	if err != nil {
		return nil, toStatus(ctx, err)
	}
	return &orderv1.GetOrderResponse{Order: toProto(order)}, nil
}

func (s *Server) ListOrders(ctx context.Context, req *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	var customerID uuid.UUID // the viewer's own orders by default; support may pass any customer
	if raw := req.GetCustomerId(); raw != "" {
		var err error
		if customerID, err = uuid.Parse(raw); err != nil {
			return nil, toStatus(ctx, &domain.ValidationError{Field: "customer_id", Message: "must be a valid UUID"})
		}
	}
	result, err := s.service.ListByCustomer(ctx, application.ListOrdersInput{
		Viewer:     viewer(ctx),
		CustomerID: customerID,
		Limit:      int(req.GetLimit()),
		Offset:     int(req.GetOffset()),
	})
	if err != nil {
		return nil, toStatus(ctx, err)
	}
	orders := make([]*orderv1.OrderSummary, 0, len(result.Orders))
	for _, order := range result.Orders {
		orders = append(orders, &orderv1.OrderSummary{
			Id:          order.ID.String(),
			Status:      toProtoStatus(order.Status),
			TotalAmount: order.TotalAmount,
			Currency:    order.Currency,
			CreateTime:  timestamppb.New(order.CreatedAt),
		})
	}
	return &orderv1.ListOrdersResponse{Orders: orders, Limit: int32(result.Limit), Offset: int32(result.Offset)}, nil
}

// toStatus maps application errors to gRPC status codes with a stable ErrorInfo reason.
func toStatus(ctx context.Context, err error) error {
	var validationErr *domain.ValidationError
	switch {
	case errors.As(err, &validationErr):
		return grpcx.FieldError(ErrorDomain, ReasonValidationFailed, validationErr.Field, validationErr.Message)
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		return grpcx.FieldError(ErrorDomain, ReasonIdempotencyKeyReused, "idempotency_key", "was already used for a different request")
	case errors.Is(err, domain.ErrNotFound):
		return grpcx.Error(codes.NotFound, ErrorDomain, ReasonOrderNotFound, "order not found")
	case errors.Is(err, domain.ErrForbidden):
		return grpcx.Error(codes.PermissionDenied, ErrorDomain, ReasonForbidden, "access to another customer's orders is denied")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "the caller canceled the call")
	case postgres.IsUnavailable(err):
		// A temporary condition, not a bug: callers retry (read methods are retried automatically).
		logger.FromContext(ctx).Error("database unavailable", "error", err)
		return grpcx.Error(codes.Unavailable, ErrorDomain, ReasonDatabaseUnavailable, "service is temporarily unavailable, retry later")
	default:
		logger.FromContext(ctx).Error("call failed", "error", err)
		errtrack.CaptureError(ctx, err, nil) // unexpected: needs a human
		return status.Error(codes.Internal, "internal error")
	}
}

// mustPrincipal returns the authenticated user; the server runs with the auth interceptor.
func mustPrincipal(ctx context.Context) auth.Principal {
	p, ok := auth.FromContext(ctx)
	if !ok {
		panic("order gRPC server runs without the auth interceptor") // a wiring bug, not a client error
	}
	return p
}

func viewer(ctx context.Context) application.Viewer {
	p := mustPrincipal(ctx)
	return application.Viewer{CustomerID: p.UserID, SeeAll: p.HasRole(auth.RoleSupport)}
}

func toProto(order *domain.Order) *orderv1.Order {
	items := make([]*orderv1.OrderItem, 0, len(order.Items))
	for _, item := range order.Items {
		items = append(items, &orderv1.OrderItem{ProductId: item.ProductID, Quantity: int32(item.Quantity), Price: item.Price})
	}
	return &orderv1.Order{
		Id:          order.ID.String(),
		CustomerId:  order.CustomerID.String(),
		Status:      toProtoStatus(order.Status),
		Items:       items,
		TotalAmount: order.TotalAmount,
		Currency:    order.Currency,
		CreateTime:  timestamppb.New(order.CreatedAt),
		UpdateTime:  timestamppb.New(order.UpdatedAt),
	}
}

func toProtoStatus(s domain.Status) orderv1.OrderStatus {
	switch s {
	case domain.StatusPending:
		return orderv1.OrderStatus_ORDER_STATUS_PENDING
	case domain.StatusPaid:
		return orderv1.OrderStatus_ORDER_STATUS_PAID
	case domain.StatusPaymentFailed:
		return orderv1.OrderStatus_ORDER_STATUS_PAYMENT_FAILED
	case domain.StatusShipped:
		return orderv1.OrderStatus_ORDER_STATUS_SHIPPED
	}
	return orderv1.OrderStatus_ORDER_STATUS_UNSPECIFIED
}
