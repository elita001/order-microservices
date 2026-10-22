package idempotency

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Results of requests with an Idempotency-Key.
const (
	resultFirst      = "first"       // executed upstream
	resultReplayed   = "replayed"    // the stored response was returned
	resultInProgress = "in_progress" // 409
	resultKeyReused  = "key_reused"  // 422
	resultBypassed   = "bypassed"    // Redis unavailable, the upstream service deduplicates
)

var requests = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "gateway_idempotency_requests_total",
	Help: "Requests with an Idempotency-Key, by result.",
}, []string{"result"})
