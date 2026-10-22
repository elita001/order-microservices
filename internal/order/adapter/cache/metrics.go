package cache

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var requests = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "order_cache_requests_total",
	Help: "Order cache lookups by result: hit, miss, or unavailable (Redis error or open breaker).",
}, []string{"result"})
