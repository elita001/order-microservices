// Package customer contains adapters for the customer directory.
package customer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	"github.com/elita001/order-microservices/internal/notification/application"
	"github.com/elita001/order-microservices/pkg/breaker"
)

// Client reads customers' emails from the customer service over gRPC. Calls are bounded by
// the connection's default deadline and retried on UNAVAILABLE by gRPC itself; a circuit
// breaker stops calling while the service is down, and the event goes to the retry queues.
type Client struct {
	client  customerv1.CustomerServiceClient
	breaker *breaker.Breaker[string]
}

var _ application.CustomerDirectory = (*Client)(nil)

func NewClient(conn grpc.ClientConnInterface, cfg breaker.Config, log *slog.Logger) *Client {
	return &Client{
		client:  customerv1.NewCustomerServiceClient(conn),
		breaker: breaker.New[string]("customer-service", cfg, isFailure, log),
	}
}

func (c *Client) Email(ctx context.Context, customerID uuid.UUID) (string, error) {
	return c.breaker.Execute(func() (string, error) {
		resp, err := c.client.GetCustomer(ctx, &customerv1.GetCustomerRequest{CustomerId: customerID.String()})
		switch status.Code(err) {
		case codes.OK:
		case codes.NotFound:
			return "", fmt.Errorf("%w: %s", application.ErrUnknownCustomer, customerID)
		default:
			return "", fmt.Errorf("customer service: %w", err)
		}
		email := resp.GetCustomer().GetEmail()
		if email == "" {
			return "", fmt.Errorf("%w: customer %s has no email", application.ErrInvalidRecipient, customerID)
		}
		return email, nil
	})
}

// isFailure counts only errors that mean the customer service is unhealthy. An unknown
// customer, or a rejected token (a configuration error), is not a reason to stop calling.
func isFailure(err error) bool {
	if errors.Is(err, application.ErrUnknownCustomer) || errors.Is(err, application.ErrInvalidRecipient) {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Internal, codes.Unknown:
		return true
	}
	return false
}
