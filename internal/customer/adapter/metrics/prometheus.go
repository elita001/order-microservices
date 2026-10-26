// Package metrics implements the customer service's business metrics with Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/elita001/order-microservices/internal/customer/application"
)

var lookups = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "customer_lookups_total",
	Help: "Customer profile lookups by result: fresh (local copy), refreshed (from Keycloak), stale (Keycloak failed, old copy served), not_found, failed.",
}, []string{"result"})

type Prometheus struct{}

var _ application.Metrics = Prometheus{}

func (Prometheus) CustomerLookup(result string) {
	lookups.WithLabelValues(result).Inc()
}
