package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/sync/errgroup"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	customergrpc "github.com/elita001/order-microservices/internal/customer/adapter/grpc"
	"github.com/elita001/order-microservices/internal/customer/adapter/keycloak"
	customermetrics "github.com/elita001/order-microservices/internal/customer/adapter/metrics"
	customerpostgres "github.com/elita001/order-microservices/internal/customer/adapter/postgres"
	"github.com/elita001/order-microservices/internal/customer/application"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/errtrack"
	"github.com/elita001/order-microservices/pkg/grpcx"
	"github.com/elita001/order-microservices/pkg/httpserver"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/metrics"
	"github.com/elita001/order-microservices/pkg/postgres"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

const serviceName = "customer-service"

type config struct {
	GRPCAddr string `env:"GRPC_ADDR" envDefault:":9085"` // the API (orderflow.customer.v1.CustomerService)
	// HTTPAddr serves only health probes and metrics.
	HTTPAddr        string        `env:"HTTP_ADDR" envDefault:":8085"`
	PostgresDSN     string        `env:"POSTGRES_DSN,required"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`
	// The longest a call may run, whatever deadline the caller sent.
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"3s"`
	// Lets grpcurl list the API. Development only.
	GRPCReflection bool `env:"GRPC_REFLECTION" envDefault:"false"`

	Telemetry telemetry.Config // OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_TRACES_SAMPLER_RATIO
	ErrTrack  errtrack.Config  // SENTRY_DSN, SENTRY_ENVIRONMENT, SENTRY_RELEASE

	Auth     auth.Config        `envPrefix:"OIDC_"`
	Keycloak keycloak.Config    `envPrefix:"KEYCLOAK_"`
	Profiles application.Config `envPrefix:"PROFILE_"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("customer service stopped with error", "error", err)
		os.Exit(1)
	}
}

// run is separate from main to ensure deferred calls execute before os.Exit.
func run() error {
	cfg, err := env.ParseAs[config]()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.LogLevel).With("service", "customer")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, serviceName, cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	defer func() {
		// Send the spans still in the buffer; ctx is already canceled at this point.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			log.Warn("flush traces", "error", err)
		}
	}()
	flushErrors, err := errtrack.Init(serviceName, cfg.ErrTrack, log)
	if err != nil {
		return fmt.Errorf("set up error tracking: %w", err)
	}
	defer flushErrors()

	pool, err := postgres.Connect(ctx, cfg.PostgresDSN, log)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	svc := application.NewService(
		customerpostgres.NewRepository(pool),
		keycloak.New(ctx, cfg.Keycloak, log),
		cfg.Profiles,
		log,
	).WithMetrics(customermetrics.Prometheus{})

	// Keycloak is not part of the health check: profiles are served from the local copies
	// while it is down, so the service stays useful.
	grpcSrv := grpcx.NewServer(grpcx.ServerOptions{
		Logger:     log,
		Verifier:   auth.NewVerifier(ctx, cfg.Auth),
		Timeout:    cfg.RequestTimeout,
		Reflection: cfg.GRPCReflection,
	})
	customerv1.RegisterCustomerServiceServer(grpcSrv, customergrpc.NewServer(svc))
	health := grpcx.RegisterHealth(grpcSrv, pool.Ping, log)

	// Internal port: Prometheus scrapes it directly, Docker probes it.
	r := chi.NewRouter()
	r.Use(middleware.RequestID, httpx.Logger(log), metrics.HTTP, telemetry.RouteName, httpx.Recoverer)
	r.Handle("/metrics", metrics.Handler())
	httpx.RegisterHealth(r, pool.Ping)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           telemetry.HTTPHandler(r),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// All components stop together: on a signal or when any of them fails.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcx.Serve(gctx, grpcSrv, cfg.GRPCAddr, cfg.ShutdownTimeout, log) })
	g.Go(func() error { return health.Run(gctx) })
	g.Go(func() error { return httpserver.Run(gctx, srv, cfg.ShutdownTimeout, log) })
	return g.Wait()
}
