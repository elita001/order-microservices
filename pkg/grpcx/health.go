package grpcx

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	healthInterval = 5 * time.Second
	healthTimeout  = 2 * time.Second
)

// Health implements the standard gRPC health protocol (grpc.health.v1), which load
// balancers, Kubernetes gRPC probes, and grpc-health-probe understand. The server reports
// SERVING while check (e.g., a database ping) passes, and NOT_SERVING once shutdown starts,
// so clients stop sending new calls before the server goes away.
type Health struct {
	server *health.Server
	check  func(context.Context) error
	log    *slog.Logger
}

// RegisterHealth registers the health service on srv; Run keeps its status up to date.
func RegisterHealth(srv *grpc.Server, check func(context.Context) error, log *slog.Logger) *Health {
	h := &Health{server: health.NewServer(), check: check, log: log}
	h.server.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING) // until the first check
	healthpb.RegisterHealthServer(srv, h.server)
	return h
}

// Run checks the dependencies every few seconds until ctx is canceled.
func (h *Health) Run(ctx context.Context) error {
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	current := healthpb.HealthCheckResponse_NOT_SERVING
	for {
		next := healthpb.HealthCheckResponse_SERVING
		checkCtx, cancel := context.WithTimeout(ctx, healthTimeout)
		err := h.check(checkCtx)
		cancel()
		if ctx.Err() != nil {
			h.server.Shutdown() // NOT_SERVING for every service, and it stays so
			return nil
		}
		if err != nil {
			next = healthpb.HealthCheckResponse_NOT_SERVING
		}
		if next != current {
			h.log.Info("grpc health status changed", "status", next.String(), "error", err)
			current = next
		}
		h.server.SetServingStatus("", next)

		select {
		case <-ctx.Done():
			h.server.Shutdown()
			return nil
		case <-ticker.C:
		}
	}
}
