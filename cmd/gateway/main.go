package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/elita001/order-microservices/internal/gateway"
	"github.com/elita001/order-microservices/internal/gateway/idempotency"
	"github.com/elita001/order-microservices/internal/gateway/proxy"
	"github.com/elita001/order-microservices/internal/gateway/ratelimit"
	"github.com/elita001/order-microservices/pkg/httpserver"
	"github.com/elita001/order-microservices/pkg/logger"
	"github.com/elita001/order-microservices/pkg/redisclient"
)

type config struct {
	HTTPAddr        string        `env:"HTTP_ADDR" envDefault:":8080"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`

	OrderServiceURL string `env:"ORDER_SERVICE_URL,required"`

	// Per client IP, requests per second and burst size.
	ReadRateLimit  int `env:"RATE_LIMIT_READ_RPS" envDefault:"50"`
	ReadRateBurst  int `env:"RATE_LIMIT_READ_BURST" envDefault:"100"`
	WriteRateLimit int `env:"RATE_LIMIT_WRITE_RPS" envDefault:"5"`
	WriteRateBurst int `env:"RATE_LIMIT_WRITE_BURST" envDefault:"10"`

	Redis       redisclient.Config `envPrefix:"REDIS_"`
	Upstream    proxy.Config       `envPrefix:"UPSTREAM_"`
	Idempotency idempotency.Config `envPrefix:"IDEMPOTENCY_"`
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
	orderURL, err := url.Parse(cfg.OrderServiceURL)
	if err != nil {
		return fmt.Errorf("parse ORDER_SERVICE_URL: %w", err)
	}

	log := logger.New(cfg.LogLevel).With("service", "gateway")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	router := gateway.NewRouter(gateway.Config{
		OrderServiceURL: orderURL,
		Upstream:        cfg.Upstream,
		ReadLimit:       ratelimit.Policy{Name: "read", Rate: cfg.ReadRateLimit, Burst: cfg.ReadRateBurst},
		WriteLimit:      ratelimit.Policy{Name: "write", Rate: cfg.WriteRateLimit, Burst: cfg.WriteRateBurst},
		Idempotency:     cfg.Idempotency,
		RedisTimeout:    cfg.Redis.OperationTimeout,
	}, rdb, log)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Longer than the upstream timeout, so the gateway can still send its 504.
		WriteTimeout: cfg.Upstream.Timeout + 5*time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return httpserver.Run(ctx, srv, cfg.ShutdownTimeout, log)
}
