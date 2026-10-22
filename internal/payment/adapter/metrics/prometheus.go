// Package metrics implements the payment service's business metrics with Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/elita001/order-microservices/internal/payment/application"
	"github.com/elita001/order-microservices/internal/payment/domain"
)

var (
	payments = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payments_total",
		Help: "Completed payments, by status and decline reason (empty if succeeded).",
	}, []string{"status", "reason"})

	// Money is counted in minor units (kopecks, cents), like everywhere in the system.
	amount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payment_amount_minor_units_total",
		Help: "Amount of completed payments in minor currency units, by currency and status.",
	}, []string{"currency", "status"})
)

type Prometheus struct{}

var _ application.Metrics = Prometheus{}

func (Prometheus) PaymentCompleted(status domain.Status, reason, currency string, minorUnits int64) {
	payments.WithLabelValues(string(status), reason).Inc()
	amount.WithLabelValues(currency, string(status)).Add(float64(minorUnits))
}
