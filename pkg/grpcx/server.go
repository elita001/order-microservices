// Package grpcx sets up gRPC servers and clients the same way in every service: tracing,
// request IDs, logs, metrics, panic recovery, deadlines, authentication, and health checks.
//
// Interceptors run in this order on the server (outermost first):
//
//	otelgrpc (stats handler) → request ID → no-retry → log + metrics → recovery → deadline → auth → handler
//
// so a recovered panic is logged and counted as INTERNAL, and a rejected token as UNAUTHENTICATED.
package grpcx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/elita001/order-microservices/pkg/auth"
)

// ServerOptions configures NewServer.
type ServerOptions struct {
	Logger *slog.Logger
	// Verifier authenticates every call except health checks and reflection; nil disables
	// authentication. OnAuthFailure, if set, is called with the reason (for metrics).
	Verifier      *auth.Verifier
	OnAuthFailure func(reason string)
	// Timeout caps how long a call may run, whatever deadline the caller sent (or none);
	// 0 keeps the caller's deadline.
	Timeout time.Duration
	// Reflection lets tools like grpcurl list services and methods. Development only:
	// it publishes the whole API schema.
	Reflection bool
}

// NewServer creates a server with the standard interceptors. Register the services on it,
// then call Serve.
func NewServer(o ServerOptions) *grpc.Server {
	metrics := serverMetrics()
	unary := []grpc.UnaryServerInterceptor{
		requestIDInterceptor,
		noRetryInterceptor,
		logInterceptor(o.Logger),
		metrics.UnaryServerInterceptor(),
		recoveryInterceptor,
	}
	if o.Timeout > 0 {
		unary = append(unary, timeoutInterceptor(o.Timeout))
	}
	if o.Verifier != nil {
		unary = append(unary, authInterceptor(o.Verifier, o.OnAuthFailure))
	}

	srv := grpc.NewServer(
		// Continues the caller's trace from the traceparent metadata.
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(unary...),
		// The only streaming method is the health Watch: metrics and recovery are enough.
		grpc.ChainStreamInterceptor(metrics.StreamServerInterceptor(), recoveryStreamInterceptor),
		// Clients ping idle connections to detect dead peers; the server tolerates pings
		// every 10s or less often and closes connections of clients that ping more.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			// Long-lived connections are closed now and then (after the grace period for
			// calls in progress), so clients reconnect and spread over new replicas.
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 30 * time.Second,
		}),
	)
	if o.Reflection {
		reflection.Register(srv)
	}
	return srv
}

// Serve blocks until ctx is canceled (SIGINT/SIGTERM) or the server fails. On cancellation,
// it stops accepting calls and gives the active ones shutdownTimeout to complete.
func Serve(ctx context.Context, srv *grpc.Server, addr string, shutdownTimeout time.Duration, log *slog.Logger) error {
	// Zero metric series for every method, so rates and ratios work before the first call.
	serverMetrics().InitializeMetrics(srv)

	lis, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("grpc server: listen %s: %w", addr, err)
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("grpc server started", "addr", addr)
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("grpc server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down grpc server")
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownTimeout):
		log.Warn("grpc calls did not finish in time, closing connections")
		srv.Stop()
	}
	log.Info("grpc server stopped")
	return nil
}
