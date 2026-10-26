// Package grpc provides the gRPC delivery adapter for customer use cases
// (orderflow.customer.v1.CustomerService).
package grpc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	"github.com/elita001/order-microservices/internal/customer/application"
	"github.com/elita001/order-microservices/internal/customer/domain"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/errtrack"
	"github.com/elita001/order-microservices/pkg/grpcx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/postgres"
)

// ErrorDomain is the google.rpc.ErrorInfo domain of this service's errors.
const ErrorDomain = "customer.orderflow"

// Error reasons (google.rpc.ErrorInfo.reason): a stable part of the API contract.
const (
	ReasonValidationFailed     = "VALIDATION_FAILED"
	ReasonCustomerNotFound     = "CUSTOMER_NOT_FOUND"
	ReasonDirectoryUnavailable = "DIRECTORY_UNAVAILABLE"
	ReasonDatabaseUnavailable  = "DATABASE_UNAVAILABLE"
)

type Service interface {
	Get(ctx context.Context, viewer application.Viewer, id uuid.UUID) (*domain.Customer, error)
}

type Server struct {
	customerv1.UnimplementedCustomerServiceServer
	service Service
}

var _ customerv1.CustomerServiceServer = (*Server)(nil)

func NewServer(service Service) *Server {
	return &Server{service: service}
}

func (s *Server) GetCustomer(ctx context.Context, req *customerv1.GetCustomerRequest) (*customerv1.GetCustomerResponse, error) {
	id, err := uuid.Parse(req.GetCustomerId())
	if err != nil {
		return nil, grpcx.FieldError(ErrorDomain, ReasonValidationFailed, "customer_id", "must be a valid UUID")
	}
	c, err := s.service.Get(ctx, viewer(ctx), id)
	if err != nil {
		return nil, toStatus(ctx, err)
	}
	return &customerv1.GetCustomerResponse{Customer: &customerv1.Customer{
		Id:        c.ID.String(),
		Username:  c.Username,
		Email:     c.Email,
		FirstName: c.FirstName,
		LastName:  c.LastName,
		SyncTime:  timestamppb.New(c.SyncedAt),
	}}, nil
}

func toStatus(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return grpcx.Error(codes.NotFound, ErrorDomain, ReasonCustomerNotFound, "customer not found")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "the caller canceled the call")
	// Checked before the database: a Keycloak timeout also wraps context.DeadlineExceeded.
	case errors.Is(err, domain.ErrDirectoryUnavailable):
		logger.FromContext(ctx).Error("identity provider unavailable", "error", err)
		return grpcx.Error(codes.Unavailable, ErrorDomain, ReasonDirectoryUnavailable, "user directory is temporarily unavailable, retry later")
	case postgres.IsUnavailable(err):
		logger.FromContext(ctx).Error("database unavailable", "error", err)
		return grpcx.Error(codes.Unavailable, ErrorDomain, ReasonDatabaseUnavailable, "service is temporarily unavailable, retry later")
	default:
		logger.FromContext(ctx).Error("call failed", "error", err)
		errtrack.CaptureError(ctx, err, nil) // unexpected: needs a human
		return status.Error(codes.Internal, "internal error")
	}
}

func viewer(ctx context.Context) application.Viewer {
	p, ok := auth.FromContext(ctx)
	if !ok {
		panic("customer gRPC server runs without the auth interceptor") // a wiring bug, not a client error
	}
	return application.Viewer{
		CustomerID: p.UserID,
		SeeAll:     p.HasRole(auth.RoleSupport) || p.HasRole(auth.RoleCustomerReader),
	}
}
