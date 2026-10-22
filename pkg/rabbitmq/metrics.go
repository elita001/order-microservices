package rabbitmq

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Results of handling a message.
const (
	resultAck        = "ack"         // processed
	resultRetry      = "retry"       // sent to a retry queue
	resultDeadLetter = "dead_letter" // sent to the DLQ
	resultRequeue    = "requeue"     // returned to the queue because forwarding failed
)

var (
	consumedMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "messaging_consumed_messages_total",
		Help: "Consumed messages by queue, event type, and result (ack, retry, dead_letter, requeue).",
	}, []string{"queue", "event", "result"})

	processingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "messaging_processing_duration_seconds",
		Help:    "Message handler duration by queue and event type.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"queue", "event"})

	consumerPaused = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "messaging_consumer_paused",
		Help: "1 while consumption is paused because a dependency (the database) is unavailable.",
	}, []string{"queue"})
)
