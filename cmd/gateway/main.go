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
	"golang.org/x/sync/errgroup"

	customerv1 "github.com/elita001/order-microservices/gen/go/orderflow/customer/v1"
	orderv1 "github.com/elita001/order-microservices/gen/go/orderflow/order/v1"
	"github.com/elita001/order-microservices/internal/gateway"
	"github.com/elita001/order-microservices/internal/gateway/idempotency"
	"github.com/elita001/order-microservices/internal/gateway/ratelimit"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/errtrack"
	"github.com/elita001/order-microservices/pkg/grpcx"
	"github.com/elita001/order-microservices/pkg/httpserver"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/metrics"
	"github.com/elita001/order-microservices/pkg/redisclient"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

type config struct {
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
	// The gateway's main port is public, so metrics are served on a separate internal port.
	MetricsAddr     string        `env:"METRICS_ADDR" envDefault:":9080"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`

	Telemetry telemetry.Config // OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_TRACES_SAMPLER_RATIO
	ErrTrack  errtrack.Config  // SENTRY_DSN, SENTRY_ENVIRONMENT, SENTRY_RELEASE

	// The services' gRPC APIs. A call without an answer within TIMEOUT gets 504.
	OrderGRPC    grpcx.ClientConfig `envPrefix:"ORDER_GRPC_"`
	CustomerGRPC grpcx.ClientConfig `envPrefix:"CUSTOMER_GRPC_"`

	// Per user, requests per second and burst size.
	ReadRateLimit  int `env:"RATE_LIMIT_READ_RPS" envDefault:"50"`
	ReadRateBurst  int `env:"RATE_LIMIT_READ_BURST" envDefault:"100"`
	WriteRateLimit int `env:"RATE_LIMIT_WRITE_RPS" envDefault:"5"`
	WriteRateBurst int `env:"RATE_LIMIT_WRITE_BURST" envDefault:"10"`

	Redis       redisclient.Config `envPrefix:"REDIS_"`
	Idempotency idempotency.Config `envPrefix:"IDEMPOTENCY_"`
	Auth        auth.Config        `envPrefix:"OIDC_"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("gateway stopped with error", "error", err)
		os.Exit(1)
	}
}

// run is separate from main to ensure deferred calls execute before os.Exit.
func run() error {
	cfg, err := env.ParseAs[config]()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.LogLevel).With("service", "gateway")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "gateway", cfg.Telemetry)
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
	flushErrors, err := errtrack.Init("gateway", cfg.ErrTrack, log)
	if err != nil {
		return fmt.Errorf("set up error tracking: %w", err)
	}
	defer flushErrors()

	rdb, err := redisclient.New(cfg.Redis, log)
	if err != nil {
		return fmt.Errorf("create redis client: %w", err)
	}
	defer rdb.Close()
	// Redis is optional: without it the gateway still works, only without rate limits
	// and with idempotency enforced by the order service alone.
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Warn("redis is unavailable, rate limiting and fast idempotency are disabled until it recovers", "error", err)
	}

	// The gateway calls the services on the user's behalf: the user's token goes along,
	// and each service authorizes the user itself. Only reads are retried automatically:
	// a repeated CreateOrder is safe only with an idempotency key, which the client controls.
	orderConn, err := grpcx.NewClient(cfg.OrderGRPC, grpcx.ClientOptions{
		Service:      orderv1.OrderService_ServiceDesc.ServiceName,
		RetryMethods: []string{"GetOrder", "ListOrders"},
		Credentials:  grpcx.ForwardToken(),
	})
	if err != nil {
		return err
	}
	defer orderConn.Close()
	customerConn, err := grpcx.NewClient(cfg.CustomerGRPC, grpcx.ClientOptions{
		Service:      customerv1.CustomerService_ServiceDesc.ServiceName,
		RetryMethods: []string{"GetCustomer"},
		Credentials:  grpcx.ForwardToken(),
	})
	if err != nil {
		return err
	}
	defer customerConn.Close()

	router := gateway.NewRouter(gateway.Config{
		Orders:       orderv1.NewOrderServiceClient(orderConn),
		Customers:    customerv1.NewCustomerServiceClient(customerConn),
		ReadLimit:    ratelimit.Policy{Name: "read", Rate: cfg.ReadRateLimit, Burst: cfg.ReadRateBurst},
		WriteLimit:   ratelimit.Policy{Name: "write", Rate: cfg.WriteRateLimit, Burst: cfg.WriteRateBurst},
		Idempotency:  cfg.Idempotency,
		RedisTimeout: cfg.Redis.OperationTimeout,
		Verifier:     auth.NewVerifier(ctx, cfg.Auth),
	}, rdb, log)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           telemetry.PublicHTTPHandler(router), // the public entry point
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Longer than the service call timeouts, so the gateway can still send its 504.
		WriteTimeout: max(cfg.OrderGRPC.Timeout, cfg.CustomerGRPC.Timeout) + 5*time.Second,
		IdleTimeout:  60 * time.Second,
	}
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metrics.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpserver.Run(gctx, srv, cfg.ShutdownTimeout, log) })
	g.Go(func() error {
		return httpserver.Run(gctx, metricsSrv, cfg.ShutdownTimeout, log.With("server", "metrics"))
	})
	return g.Wait()
}
