//go:build integration

// Package testkit starts real dependencies for integration tests with testcontainers:
// PostgreSQL with the migrations of all services, and RabbitMQ. Containers are started once
// per test package (in TestMain) and removed afterwards; tests isolate their data with
// random IDs instead of restarting containers.
//
// Run with: make test-integration (go test -tags integration ./...). Requires Docker.
package testkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
)

// Images match docker-compose.yml.
const (
	postgresImage = "postgres:16-alpine"
	rabbitMQImage = "rabbitmq:4.1-management-alpine"
)

// Needs lists the dependencies a test package uses.
type Needs struct {
	Postgres bool
	RabbitMQ bool
}

// Env holds the addresses of the started dependencies.
type Env struct {
	PostgresDSN string
	RabbitMQURL string
}

// Current is filled by Main before the tests run.
var Current Env

// Main starts the dependencies, runs the tests, and removes the containers. Use it in TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testkit.Main(m, testkit.Needs{Postgres: true})) }
func Main(m *testing.M, needs Needs) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var containers []testcontainers.Container
	defer func() {
		for _, c := range containers {
			_ = testcontainers.TerminateContainer(c)
		}
	}()

	if needs.Postgres {
		c, dsn, err := startPostgres(ctx)
		if c != nil { // a concrete pointer: an interface holding a nil pointer is not nil
			containers = append(containers, c)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "start postgres:", err)
			return 1
		}
		Current.PostgresDSN = dsn
	}
	if needs.RabbitMQ {
		c, url, err := startRabbitMQ(ctx)
		if c != nil {
			containers = append(containers, c)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "start rabbitmq:", err)
			return 1
		}
		Current.RabbitMQURL = url
	}
	return m.Run()
}

// Pool connects to the test database; the pool is closed when the test ends.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), Current.PostgresDSN)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func startPostgres(ctx context.Context) (*tcpostgres.PostgresContainer, string, error) {
	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("orderflow"),
		tcpostgres.WithUsername("app"),
		tcpostgres.WithPassword("app"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return c, "", err
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return c, "", err
	}
	if err := migrate(ctx, dsn); err != nil {
		return c, "", err
	}
	return c, dsn, nil
}

func startRabbitMQ(ctx context.Context) (*tcrabbitmq.RabbitMQContainer, string, error) {
	c, err := tcrabbitmq.Run(ctx, rabbitMQImage)
	if err != nil {
		return c, "", err
	}
	url, err := c.AmqpURL(ctx)
	return c, url, err
}

// migrate applies the up migrations of every service in order, like the migrate
// containers of docker-compose.yml do (one database, separate tables per service).
func migrate(ctx context.Context, dsn string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, service := range []string{"order", "payment", "delivery", "notification", "customer"} {
		files, err := filepath.Glob(filepath.Join(root, "migrations", service, "*.up.sql"))
		if err != nil {
			return err
		}
		sort.Strings(files) // 000001_..., 000002_...
		for _, file := range files {
			sql, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if _, err := pool.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("apply %s: %w", filepath.Base(file), err)
			}
		}
	}
	return nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
