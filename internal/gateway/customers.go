package gateway

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/httpx"
)

// customers translates the REST API of customer profiles into calls of the customer service.
type customers struct {
	client customerv1.CustomerServiceClient
}

type customerResponse struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name"`
	SyncedAt  time.Time `json:"synced_at"`
}

// me returns the authenticated user's own profile.
func (h customers) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context()) // the route is behind auth.Middleware
	h.write(w, r, p.UserID.String())
}

// get returns any customer's profile: support only, others get 404 from the service.
func (h customers) get(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, chi.URLParam(r, "id"))
}

func (h customers) write(w http.ResponseWriter, r *http.Request, id string) {
	resp, err := h.client.GetCustomer(r.Context(), &customerv1.GetCustomerRequest{CustomerId: id})
	if err != nil {
		writeUpstreamError(w, r, "customer", err)
		return
	}
	c := resp.GetCustomer()
	httpx.WriteJSON(w, http.StatusOK, customerResponse{
		ID:        c.GetId(),
		Username:  c.GetUsername(),
		Email:     c.GetEmail(),
		FirstName: c.GetFirstName(),
		LastName:  c.GetLastName(),
		SyncedAt:  c.GetSyncTime().AsTime(),
	})
}
