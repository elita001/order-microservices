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

	orderhttp "github.com/elita001/order-microservices/internal/order/adapter/http"
	orderpostgres "github.com/elita001/order-microservices/internal/order/adapter/postgres"
	"github.com/elita001/order-microservices/internal/order/application"
	"github.com/elita001/order-microservices/pkg/httpserver"
	"github.com/elita001/order-microservices/pkg/httpx"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/postgres"
)

type config struct {
	HTTPAddr        string        `env:"HTTP_ADDR" envDefault:":8081"`
	PostgresDSN     string        `env:"POSTGRES_DSN,required"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`
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
	return httpserver.Run(ctx, srv, cfg.ShutdownTimeout, log)
}
