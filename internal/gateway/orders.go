package gateway

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	orderv1 "github.com/elita001/order-microservices/gen/go/orderflow/order/v1"
	"github.com/elita001/order-microservices/pkg/httpx"
)

// orders translates the public REST API of orders into calls of the order service (gRPC).
// The JSON format is the public contract and stays stable when the proto evolves.
type orders struct {
	client orderv1.OrderServiceClient
}

type itemDTO struct {
	ProductID string `json:"product_id"`
	// int32 as in the proto: a larger number is rejected while decoding instead of wrapping
	// around into a valid-looking quantity.
	Quantity int32 `json:"quantity"`
	Price    int64 `json:"price"`
}

// createOrderRequest has no customer_id: the customer is the authenticated user. A body
// with customer_id is rejected (unknown field), so old clients fail loudly, not silently.
type createOrderRequest struct {
	Currency string    `json:"currency"`
	Items    []itemDTO `json:"items"`
}

type orderResponse struct {
	ID          string    `json:"id"`
	CustomerID  string    `json:"customer_id"`
	Status      string    `json:"status"`
	TotalAmount int64     `json:"total_amount"`
	Currency    string    `json:"currency"`
	Items       []itemDTO `json:"items"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type orderSummaryResponse struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	TotalAmount int64     `json:"total_amount"`
	Currency    string    `json:"currency"`
	CreatedAt   time.Time `json:"created_at"`
}

type listOrdersResponse struct {
	Orders []orderSummaryResponse `json:"orders"`
	Limit  int32                  `json:"limit"`
	Offset int32                  `json:"offset"`
}

func (h orders) create(w http.ResponseWriter, r *http.Request) {
	var req createOrderRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	items := make([]*orderv1.OrderItem, 0, len(req.Items))
	for _, item := range req.Items {
		items = append(items, &orderv1.OrderItem{ProductId: item.ProductID, Quantity: item.Quantity, Price: item.Price})
	}

	resp, err := h.client.CreateOrder(r.Context(), &orderv1.CreateOrderRequest{
		Currency:       req.Currency,
		Items:          items,
		IdempotencyKey: r.Header.Get(httpx.IdempotencyKeyHeader),
	})
	if err != nil {
		writeUpstreamError(w, r, "order", err)
		return
	}
	// A replay returns the original response, so the client cannot tell it from the first one,
	// except for this informational header.
	if resp.GetReplayed() {
		w.Header().Set(httpx.IdempotentReplayedHeader, "true")
	}
	httpx.WriteJSON(w, http.StatusCreated, toOrderResponse(resp.GetOrder()))
}

func (h orders) get(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.GetOrder(r.Context(), &orderv1.GetOrderRequest{OrderId: chi.URLParam(r, "id")})
	if err != nil {
		writeUpstreamError(w, r, "order", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toOrderResponse(resp.GetOrder()))
}

func (h orders) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := queryInt32(w, q, "limit")
	if !ok {
		return
	}
	offset, ok := queryInt32(w, q, "offset")
	if !ok {
		return
	}
	resp, err := h.client.ListOrders(r.Context(), &orderv1.ListOrdersRequest{
		CustomerId: q.Get("customer_id"), // optional: support may list any customer's orders
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		writeUpstreamError(w, r, "order", err)
		return
	}
	summaries := make([]orderSummaryResponse, 0, len(resp.GetOrders()))
	for _, o := range resp.GetOrders() {
		summaries = append(summaries, orderSummaryResponse{
			ID:          o.GetId(),
			Status:      statusName(o.GetStatus()),
			TotalAmount: o.GetTotalAmount(),
			Currency:    o.GetCurrency(),
			CreatedAt:   o.GetCreateTime().AsTime(),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, listOrdersResponse{Orders: summaries, Limit: resp.GetLimit(), Offset: resp.GetOffset()})
}

func toOrderResponse(o *orderv1.Order) orderResponse {
	items := make([]itemDTO, 0, len(o.GetItems()))
	for _, item := range o.GetItems() {
		items = append(items, itemDTO{ProductID: item.GetProductId(), Quantity: item.GetQuantity(), Price: item.GetPrice()})
	}
	return orderResponse{
		ID:          o.GetId(),
		CustomerID:  o.GetCustomerId(),
		Status:      statusName(o.GetStatus()),
		TotalAmount: o.GetTotalAmount(),
		Currency:    o.GetCurrency(),
		Items:       items,
		CreatedAt:   o.GetCreateTime().AsTime(),
		UpdatedAt:   o.GetUpdateTime().AsTime(),
	}
}

// statusName maps the proto enum to the REST API's status names.
func statusName(s orderv1.OrderStatus) string {
	switch s {
	case orderv1.OrderStatus_ORDER_STATUS_PENDING:
		return "pending"
	case orderv1.OrderStatus_ORDER_STATUS_PAID:
		return "paid"
	case orderv1.OrderStatus_ORDER_STATUS_PAYMENT_FAILED:
		return "payment_failed"
	case orderv1.OrderStatus_ORDER_STATUS_SHIPPED:
		return "shipped"
	case orderv1.OrderStatus_ORDER_STATUS_UNSPECIFIED:
	}
	return "unknown" // a status added to the proto before the gateway learned it
}

// queryInt32 parses an optional integer query parameter; it writes 422 and returns false
// if the value is not an integer.
func queryInt32(w http.ResponseWriter, q url.Values, key string) (int32, bool) {
	raw := q.Get(key)
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "validation_error", key+": must be a valid integer")
		return 0, false
	}
	return int32(value), true
}
