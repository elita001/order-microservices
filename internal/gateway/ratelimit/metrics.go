package ratelimit

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var decisions = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "gateway_rate_limit_decisions_total",
	Help: "Rate limiter decisions by policy: allowed, rejected (429), or bypassed (Redis unavailable).",
}, []string{"policy", "decision"})
