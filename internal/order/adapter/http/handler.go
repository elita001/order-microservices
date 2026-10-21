// Package http provides the HTTP delivery adapter for order use cases.
package http

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
)

type Service interface {
	Create(ctx context.Context, in application.CreateOrderInput) (*application.CreateOrderResult, error)
	Get(ctx context.Context, id uuid.UUID) (*domain.Order, error)
	ListByCustomer(ctx context.Context, in application.ListOrdersInput) (*application.ListOrdersResult, error)
}

type Handler struct {
	service Service
}

func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Register(r chi.Router) {
	r.Post("/orders", h.createOrder)
	r.Get("/orders/{id}", h.getOrder)
	r.Get("/orders", h.listOrders)
}

type itemDTO struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
	Price     int64  `json:"price"`
}

type createOrderRequest struct {
	CustomerID string    `json:"customer_id"`
	Currency   string    `json:"currency"`
	Items      []itemDTO `json:"items"`
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
	Limit  int                    `json:"limit"`
	Offset int                    `json:"offset"`
}

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	var req createOrderRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	customerID, err := uuid.Parse(req.CustomerID)
	if err != nil {
		h.handleError(w, r, &domain.ValidationError{Field: "customer_id", Message: "must be a valid UUID"})
		return
	}

	items := make([]domain.Item, 0, len(req.Items))
	for _, item := range req.Items {
		items = append(items, domain.Item{ProductID: item.ProductID, Quantity: item.Quantity, Price: item.Price})
	}

	result, err := h.service.Create(r.Context(), application.CreateOrderInput{
		CustomerID:     customerID,
		Currency:       req.Currency,
		Items:          items,
		IdempotencyKey: r.Header.Get(httpx.IdempotencyKeyHeader),
	})
	if err != nil {
		h.handleError(w, r, err)
		return
	}
	// A replay returns the original response, so the client cannot tell it from the first one,
	// except for this informational header.
	if result.Replayed {
		w.Header().Set(httpx.IdempotentReplayedHeader, "true")
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(result.Order))
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.handleError(w, r, &domain.ValidationError{Field: "id", Message: "must be a valid UUID"})
		return
	}

	order, err := h.service.Get(r.Context(), id)
	if err != nil {
		h.handleError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(order))
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	customerID, err := uuid.Parse(q.Get("customer_id"))
	if err != nil {
		h.handleError(w, r, &domain.ValidationError{Field: "customer_id", Message: "must be a valid UUID"})
		return
	}
	limit, err := queryInt(q, "limit")
	if err != nil {
		h.handleError(w, r, err)
		return
	}
	offset, err := queryInt(q, "offset")
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	result, err := h.service.ListByCustomer(r.Context(), application.ListOrdersInput{
		CustomerID: customerID,
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		h.handleError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toListResponse(result))
}

func (h *Handler) handleError(w http.ResponseWriter, r *http.Request, err error) {
	var validationErr *domain.ValidationError
	switch {
	case errors.As(err, &validationErr):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "validation_error", validationErr.Error())
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "order not found")
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", err.Error())
	default:
		logger.FromContext(r.Context()).Error("request failed", "error", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func toResponse(order *domain.Order) orderResponse {
	items := make([]itemDTO, 0, len(order.Items))
	for _, item := range order.Items {
		items = append(items, itemDTO{ProductID: item.ProductID, Quantity: item.Quantity, Price: item.Price})
	}
	return orderResponse{
		ID:          order.ID.String(),
		CustomerID:  order.CustomerID.String(),
		Status:      string(order.Status),
		TotalAmount: order.TotalAmount,
		Currency:    order.Currency,
		Items:       items,
		CreatedAt:   order.CreatedAt,
		UpdatedAt:   order.UpdatedAt,
	}
}

func toListResponse(result *application.ListOrdersResult) listOrdersResponse {
	orders := make([]orderSummaryResponse, 0, len(result.Orders))
	for _, order := range result.Orders {
		orders = append(orders, orderSummaryResponse{
			ID:          order.ID.String(),
			Status:      string(order.Status),
			TotalAmount: order.TotalAmount,
			Currency:    order.Currency,
			CreatedAt:   order.CreatedAt,
		})
	}
	return listOrdersResponse{Orders: orders, Limit: result.Limit, Offset: result.Offset}
}

func queryInt(q url.Values, key string) (int, error) {
	raw := q.Get(key)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &domain.ValidationError{Field: key, Message: "must be a valid integer"}
	}
	return value, nil
}
