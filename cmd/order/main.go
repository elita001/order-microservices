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

	ordercache "github.com/elita001/order-microservices/internal/order/adapter/cache"
	orderhttp "github.com/elita001/order-microservices/internal/order/adapter/http"
	ordermessaging "github.com/elita001/order-microservices/internal/order/adapter/messaging"
	ordermetrics "github.com/elita001/order-microservices/internal/order/adapter/metrics"
	orderpostgres "github.com/elita001/order-microservices/internal/order/adapter/postgres"
	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/pkg/httpserver"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/metrics"
	"github.com/elita001/order-microservices/pkg/outbox"
	"github.com/elita001/order-microservices/pkg/postgres"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
	"github.com/elita001/order-microservices/pkg/redisclient"
)

const serviceName = "order-service"

type config struct {
	HTTPAddr        string        `env:"HTTP_ADDR" envDefault:":8081"`
	PostgresDSN     string        `env:"POSTGRES_DSN,required"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`
	// Shorter than the gateway's UPSTREAM_TIMEOUT, so the service answers itself
	// (503 if the database hangs) instead of the gateway answering 504.
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" envDefault:"3s"`

	RabbitMQURL      string `env:"RABBITMQ_URL,required"`
	RabbitMQExchange string `env:"RABBITMQ_EXCHANGE" envDefault:"orderflow.events"`
	RabbitMQQueue    string `env:"RABBITMQ_QUEUE" envDefault:"order-service"`

	Outbox   outbox.RelayConfig      `envPrefix:"OUTBOX_"`
	Consumer rabbitmq.ConsumerConfig `envPrefix:"CONSUMER_"`
	Redis    redisclient.Config      `envPrefix:"REDIS_"`
	Cache    ordercache.Config       `envPrefix:"CACHE_"`
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
	httpHandler := orderhttp.NewHandler(svc)
	eventHandler := ordermessaging.NewHandler(svc)

	consumer := rabbitmq.NewConsumer(cfg.RabbitMQURL, serviceName, rabbitmq.Topology{
		Exchange:    cfg.RabbitMQExchange,
		Queue:       cfg.RabbitMQQueue,
		RoutingKeys: eventHandler.RoutingKeys(),
	}, cfg.Consumer, log).WithHealthCheck(pool.Ping)

	r := chi.NewRouter()
	r.Use(middleware.RequestID, httpx.Logger(log), metrics.HTTP, httpx.Recoverer)
	// Internal port: Prometheus scrapes it directly, clients never reach it.
	r.Handle("/metrics", metrics.Handler())
	httpx.RegisterHealth(r, pool.Ping)
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(httpx.Timeout(cfg.RequestTimeout))
		httpHandler.Register(r)
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// All components stop together: on a signal or when any of them fails.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpserver.Run(gctx, srv, cfg.ShutdownTimeout, log) })
	g.Go(func() error { return relay.Run(gctx) })
	g.Go(func() error { return consumer.Run(gctx, eventHandler.Handle) })
	return g.Wait()
}
