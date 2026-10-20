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

	orderhttp "github.com/elita001/order-microservices/internal/order/adapter/http"
	orderpostgres "github.com/elita001/order-microservices/internal/order/adapter/postgres"
	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/pkg/httpserver"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/outbox"
	"github.com/elita001/order-microservices/pkg/postgres"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
)

type config struct {
	HTTPAddr        string        `env:"HTTP_ADDR" envDefault:":8081"`
	PostgresDSN     string        `env:"POSTGRES_DSN,required"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`

	RabbitMQURL      string `env:"RABBITMQ_URL,required"`
	RabbitMQExchange string `env:"RABBITMQ_EXCHANGE" envDefault:"orderflow.events"`

	Outbox outboxConfig `envPrefix:"OUTBOX_"`
}

type outboxConfig struct {
	BatchSize       int           `env:"BATCH_SIZE" envDefault:"100"`
	PollInterval    time.Duration `env:"POLL_INTERVAL" envDefault:"1s"`
	MaxBackoff      time.Duration `env:"MAX_BACKOFF" envDefault:"30s"`
	PublishTimeout  time.Duration `env:"PUBLISH_TIMEOUT" envDefault:"5s"`
	BatchTimeout    time.Duration `env:"BATCH_TIMEOUT" envDefault:"8s"`
	Retention       time.Duration `env:"RETENTION" envDefault:"168h"`
	CleanupInterval time.Duration `env:"CLEANUP_INTERVAL" envDefault:"1h"`
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

	publisher := rabbitmq.NewPublisher(cfg.RabbitMQURL, cfg.RabbitMQExchange, "order-service", log)
	defer publisher.Close()

	relay := outbox.NewRelay(pool, publisher, outbox.RelayConfig{
		BatchSize:       cfg.Outbox.BatchSize,
		PollInterval:    cfg.Outbox.PollInterval,
		MaxBackoff:      cfg.Outbox.MaxBackoff,
		PublishTimeout:  cfg.Outbox.PublishTimeout,
		BatchTimeout:    cfg.Outbox.BatchTimeout,
		Retention:       cfg.Outbox.Retention,
		CleanupInterval: cfg.Outbox.CleanupInterval,
	}, log)

	repo := orderpostgres.NewRepository(pool)
	svc := application.NewService(repo, log)
	handler := orderhttp.NewHandler(svc)

	r := chi.NewRouter()
	r.Use(middleware.RequestID, httpx.Logger(log), httpx.Recoverer)

	// liveness: the process is alive
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// readiness: the service is ready to accept traffic (the database is available)
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			httpx.WriteError(w, http.StatusServiceUnavailable, "not_ready", "database unavailable")
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	r.Route("/api/v1", handler.Register)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// The HTTP server and the outbox relay stop together: on a signal or when either fails.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpserver.Run(gctx, srv, cfg.ShutdownTimeout, log) })
	g.Go(func() error { return relay.Run(gctx) })
	return g.Wait()
}
