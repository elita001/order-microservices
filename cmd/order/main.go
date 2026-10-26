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

	orderv1 "github.com/elita001/order-microservices/gen/go/orderflow/order/v1"
	ordercache "github.com/elita001/order-microservices/internal/order/adapter/cache"
	ordergrpc "github.com/elita001/order-microservices/internal/order/adapter/grpc"
	ordermessaging "github.com/elita001/order-microservices/internal/order/adapter/messaging"
	ordermetrics "github.com/elita001/order-microservices/internal/order/adapter/metrics"
	orderpostgres "github.com/elita001/order-microservices/internal/order/adapter/postgres"
	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/pkg/auth"
	"github.com/elita001/order-microservices/pkg/errtrack"
	"github.com/elita001/order-microservices/pkg/grpcx"
	"github.com/elita001/order-microservices/pkg/httpserver"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/metrics"
	"github.com/elita001/order-microservices/pkg/outbox"
	"github.com/elita001/order-microservices/pkg/postgres"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
	"github.com/elita001/order-microservices/pkg/redisclient"
	"github.com/elita001/order-microservices/pkg/telemetry"
)

const serviceName = "order-service"

type config struct {
	GRPCAddr string `env:"GRPC_ADDR" envDefault:":9081"` // the API (orderflow.order.v1.OrderService)
	// HTTPAddr serves only health probes and metrics.
	HTTPAddr        string        `env:"HTTP_ADDR" envDefault:":8081"`
	PostgresDSN     string        `env:"POSTGRES_DSN,required"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`

	Telemetry telemetry.Config // OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_TRACES_SAMPLER_RATIO
	ErrTrack  errtrack.Config  // SENTRY_DSN, SENTRY_ENVIRONMENT, SENTRY_RELEASE
	// The longest a call may run, whatever deadline the caller sent. Callers usually send a
	// shorter one (the gateway's ORDER_GRPC_TIMEOUT), which the server keeps.
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"3s"`
	// Lets grpcurl list the API. Development only.
	GRPCReflection bool `env:"GRPC_REFLECTION" envDefault:"false"`

	RabbitMQURL      string `env:"RABBITMQ_URL,required"`
	RabbitMQExchange string `env:"RABBITMQ_EXCHANGE" envDefault:"orderflow.events"`
	RabbitMQQueue    string `env:"RABBITMQ_QUEUE" envDefault:"order-service"`

	Outbox   outbox.RelayConfig      `envPrefix:"OUTBOX_"`
	Consumer rabbitmq.ConsumerConfig `envPrefix:"CONSUMER_"`
	Redis    redisclient.Config      `envPrefix:"REDIS_"`
	Cache    ordercache.Config       `envPrefix:"CACHE_"`
	Auth     auth.Config             `envPrefix:"OIDC_"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("order service stopped with error", "error", err)
		os.Exit(1)
	}
}

// run is separate from main to ensure deferred calls execute before os.Exit.
func run() error {
	cfg, err := env.ParseAs[config]()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.LogLevel).With("service", "order")
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

	publisher := rabbitmq.NewPublisher(cfg.RabbitMQURL, cfg.RabbitMQExchange, serviceName, log)
	defer publisher.Close()
	relay := outbox.NewRelay(pool, orderpostgres.OutboxTable, publisher, cfg.Outbox, log)

	rdb, err := redisclient.New(cfg.Redis, log)
	if err != nil {
		return fmt.Errorf("create redis client: %w", err)
	}
	defer rdb.Close()
	// The cache is optional: without Redis, orders are read from the database.
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Warn("redis is unavailable, the order cache is bypassed until it recovers", "error", err)
	}

	// Cache-aside decorator: the application service does not know about the cache.
	repo := ordercache.New(orderpostgres.NewRepository(pool), rdb, cfg.Cache, cfg.Redis.OperationTimeout, log)
	svc := application.NewService(repo, log).WithMetrics(ordermetrics.Prometheus{})
	eventHandler := ordermessaging.NewHandler(svc)

	consumer := rabbitmq.NewConsumer(cfg.RabbitMQURL, serviceName, rabbitmq.Topology{
		Exchange:    cfg.RabbitMQExchange,
		Queue:       cfg.RabbitMQQueue,
		RoutingKeys: eventHandler.RoutingKeys(),
	}, cfg.Consumer, log).WithHealthCheck(pool.Ping)

	// The service verifies the access token itself instead of trusting the gateway: its port
	// can be reached directly, and identity must not come from metadata a caller can set.
	grpcSrv := grpcx.NewServer(grpcx.ServerOptions{
		Logger:     log,
		Verifier:   auth.NewVerifier(ctx, cfg.Auth),
		Timeout:    cfg.RequestTimeout,
		Reflection: cfg.GRPCReflection,
	})
	orderv1.RegisterOrderServiceServer(grpcSrv, ordergrpc.NewServer(svc))
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
	g.Go(func() error { return relay.Run(gctx) })
	g.Go(func() error { return consumer.Run(gctx, eventHandler.Handle) })
	return g.Wait()
}
