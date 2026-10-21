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

	"github.com/elita001/order-microservices/internal/notification/adapter/customer"
	notificationmessaging "github.com/elita001/order-microservices/internal/notification/adapter/messaging"
	notificationpostgres "github.com/elita001/order-microservices/internal/notification/adapter/postgres"
	"github.com/elita001/order-microservices/internal/notification/adapter/smtp"
	"github.com/elita001/order-microservices/internal/notification/application"
	"github.com/elita001/order-microservices/pkg/httpserver"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/postgres"
	"github.com/elita001/order-microservices/pkg/rabbitmq"
)

const serviceName = "notification-service"

type config struct {
	HTTPAddr        string        `env:"HTTP_ADDR" envDefault:":8084"`
	PostgresDSN     string        `env:"POSTGRES_DSN,required"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`

	RabbitMQURL      string `env:"RABBITMQ_URL,required"`
	RabbitMQExchange string `env:"RABBITMQ_EXCHANGE" envDefault:"orderflow.events"`
	RabbitMQQueue    string `env:"RABBITMQ_QUEUE" envDefault:"notification-service"`

	Consumer rabbitmq.ConsumerConfig `envPrefix:"CONSUMER_"`
	SMTP     smtp.Config             `envPrefix:"SMTP_"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("notification service stopped with error", "error", err)
		os.Exit(1)
	}
}

// run is separate from main to ensure deferred calls execute before os.Exit.
func run() error {
	cfg, err := env.ParseAs[config]()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.LogLevel).With("service", "notification")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.PostgresDSN, log)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	repo := notificationpostgres.NewRepository(pool)
	svc := application.NewService(repo, customer.Stub{}, smtp.NewMailer(cfg.SMTP), log)
	eventHandler := notificationmessaging.NewHandler(svc)

	consumer := rabbitmq.NewConsumer(cfg.RabbitMQURL, serviceName, rabbitmq.Topology{
		Exchange:    cfg.RabbitMQExchange,
		Queue:       cfg.RabbitMQQueue,
		RoutingKeys: eventHandler.RoutingKeys(),
	}, cfg.Consumer, log).WithHealthCheck(pool.Ping)

	// The notification service has no public API; HTTP serves health probes
	// (and metrics in a later step).
	r := chi.NewRouter()
	r.Use(middleware.RequestID, httpx.Logger(log), httpx.Recoverer)
	httpx.RegisterHealth(r, pool.Ping)

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
	g.Go(func() error { return consumer.Run(gctx, eventHandler.Handle) })
	return g.Wait()
}
