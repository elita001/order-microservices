// Package metrics implements the order service's business metrics with Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/internal/order/domain"
)

var (
	ordersCreated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orders_created_total",
		Help: "Orders created, by currency (replayed requests are not counted).",
	}, []string{"currency"})

	statusChanges = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "order_status_changes_total",
		Help: "Order status changes, by the new status.",
	}, []string{"status"})
)

type Prometheus struct{}

var _ application.Metrics = Prometheus{}

func (Prometheus) OrderCreated(currency string) {
	ordersCreated.WithLabelValues(currency).Inc()
}

func (Prometheus) OrderStatusChanged(status domain.Status) {
	statusChanges.WithLabelValues(string(status)).Inc()
}
