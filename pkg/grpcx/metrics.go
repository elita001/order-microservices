package grpcx

import (
	"sync"

	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
)

// The same buckets as the HTTP metrics, so the latencies are comparable on one panel.
var latencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// RED metrics of gRPC calls, with the standard names of go-grpc-middleware:
// grpc_server_handled_total{grpc_service, grpc_method, grpc_code}, grpc_server_handling_seconds,
// and the same for clients (grpc_client_*). Labels are bounded: methods come from the
// proto services, codes are the 17 gRPC codes.
var (
	serverMetrics = sync.OnceValue(func() *grpcprom.ServerMetrics {
		m := grpcprom.NewServerMetrics(grpcprom.WithServerHandlingTimeHistogram(grpcprom.WithHistogramBuckets(latencyBuckets)))
		prometheus.MustRegister(m)
		return m
	})
	clientMetrics = sync.OnceValue(func() *grpcprom.ClientMetrics {
		m := grpcprom.NewClientMetrics(grpcprom.WithClientHandlingTimeHistogram(grpcprom.WithHistogramBuckets(latencyBuckets)))
		prometheus.MustRegister(m)
		return m
	})
)
