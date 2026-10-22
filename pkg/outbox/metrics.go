package outbox

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	publishedMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_published_messages_total",
		Help: "Messages published from the outbox table.",
	}, []string{"table"})

	publishErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_publish_errors_total",
		Help: "Failed attempts to publish an outbox message.",
	}, []string{"table"})

	pendingMessages = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "outbox_pending_messages",
		Help: "Messages waiting in the outbox table.",
	}, []string{"table"})

	// The key metric of an outbox: how late events are. It grows while the broker is down.
	oldestPendingAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "outbox_oldest_pending_age_seconds",
		Help: "Age of the oldest unpublished message, 0 if the outbox is empty.",
	}, []string{"table"})
)
