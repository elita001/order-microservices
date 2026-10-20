// Package customer contains adapters for the customer directory.
package customer

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/notification/application"
)

// Stub simulates an external customer service. Every customer gets a predictable
// address in the reserved example.com domain, so emails can never reach a real person.
type Stub struct{}

var _ application.CustomerDirectory = Stub{}

func (Stub) Email(_ context.Context, customerID uuid.UUID) (string, error) {
	return fmt.Sprintf("customer-%s@example.com", customerID.String()[:8]), nil
}
