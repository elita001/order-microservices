// Package metrics implements the notification service's business metrics with Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/elita001/order-microservices/internal/notification/application"
	"github.com/elita001/order-microservices/internal/notification/domain"
)

var sent = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "notifications_sent_total",
	Help: "Emails sent to customers, by kind.",
}, []string{"kind"})

type Prometheus struct{}

var _ application.Metrics = Prometheus{}

func (Prometheus) NotificationSent(kind domain.Kind) {
	sent.WithLabelValues(string(kind)).Inc()
}
