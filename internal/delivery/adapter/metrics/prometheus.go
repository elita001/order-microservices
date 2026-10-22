// Package metrics implements the delivery service's business metrics with Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/elita001/order-microservices/internal/delivery/application"
)

var shipments = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "shipments_registered_total",
	Help: "Shipments handed over to a carrier, by carrier.",
}, []string{"carrier"})

type Prometheus struct{}

var _ application.Metrics = Prometheus{}

func (Prometheus) ShipmentRegistered(carrier string) {
	shipments.WithLabelValues(carrier).Inc()
}
