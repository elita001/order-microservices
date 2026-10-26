// Package application implements customer use cases and defines their ports.
package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/elita001/order-microservices/internal/customer/domain"
)

// Repository stores the local copies of customer profiles.
type Repository interface {
	// Get returns domain.ErrNotFound if there is no local copy.
	Get(ctx context.Context, id uuid.UUID) (*domain.Customer, error)
	// Save inserts or replaces the copy.
	Save(ctx context.Context, c *domain.Customer) error
	Delete(ctx context.Context, id uuid.UUID) error
}

// Directory is the identity provider's user directory, the source of truth for identities.
type Directory interface {
	// User returns domain.ErrNotFound if the identity provider has no such user.
	User(ctx context.Context, id uuid.UUID) (*domain.Customer, error)
}

// Logger is the logging capability required by the application service.
type Logger interface {
	InfoContext(ctx context.Context, message string, args ...any)
	WarnContext(ctx context.Context, message string, args ...any)
}

// Lookup results for metrics.
const (
	LookupFresh     = "fresh"     // the local copy was recent enough
	LookupRefreshed = "refreshed" // fetched from the identity provider
	LookupStale     = "stale"     // the identity provider failed, an old copy was served
	LookupNotFound  = "not_found"
	LookupFailed    = "failed" // the identity provider failed and there was no copy
)

// Metrics records business metrics; a Prometheus adapter implements it.
type Metrics interface {
	CustomerLookup(result string)
}

type Config struct {
	// How long a local copy is served without asking the identity provider. Longer means
	// fewer calls to it, but changes (a new email) reach the services later.
	RefreshAfter time.Duration `env:"REFRESH_AFTER" envDefault:"1h"`
}

type Service struct {
	repo      Repository
	directory Directory
	cfg       Config
	logger    Logger
	metrics   Metrics
	now       func() time.Time
}

func NewService(repo Repository, directory Directory, cfg Config, logger Logger) *Service {
	return &Service{
		repo:      repo,
		directory: directory,
		cfg:       cfg,
		logger:    logger,
		metrics:   noopMetrics{},
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// WithMetrics enables business metrics; without it they are not recorded.
func (s *Service) WithMetrics(m Metrics) *Service {
	s.metrics = m
	return s
}

type noopMetrics struct{}

func (noopMetrics) CustomerLookup(string) {}

// Viewer is who reads a profile: a customer sees only their own; support and services
// with the customer-reader role (e.g., notifications) see all.
type Viewer struct {
	CustomerID uuid.UUID
	SeeAll     bool
}

// Get returns a customer's profile. A recent local copy is served as is; an older one is
// refreshed from the identity provider, and served stale if the provider is unavailable.
func (s *Service) Get(ctx context.Context, viewer Viewer, id uuid.UUID) (*domain.Customer, error) {
	// Someone else's profile is "not found", so its existence is not revealed.
	if !viewer.SeeAll && viewer.CustomerID != id {
		return nil, domain.ErrNotFound
	}

	local, err := s.repo.Get(ctx, id)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("get customer: %w", err)
	}
	if local != nil && s.now().Sub(local.SyncedAt) < s.cfg.RefreshAfter {
		s.metrics.CustomerLookup(LookupFresh)
		return local, nil
	}

	fresh, err := s.directory.User(ctx, id)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		s.metrics.CustomerLookup(LookupNotFound)
		if local != nil {
			// The user was deleted in the identity provider: the copy of their personal data goes too.
			if err := s.repo.Delete(ctx, id); err != nil {
				return nil, fmt.Errorf("delete customer: %w", err)
			}
			s.logger.InfoContext(ctx, "customer deleted: the user no longer exists in the identity provider", "customer_id", id)
		}
		return nil, domain.ErrNotFound
	case err != nil && local != nil:
		s.metrics.CustomerLookup(LookupStale)
		s.logger.WarnContext(ctx, "identity provider unavailable, serving a stale customer profile",
			"customer_id", id, "synced_at", local.SyncedAt, "error", err)
		return local, nil
	case err != nil:
		s.metrics.CustomerLookup(LookupFailed)
		return nil, fmt.Errorf("%w: %w", domain.ErrDirectoryUnavailable, err)
	}

	fresh.SyncedAt = s.now()
	if err := s.repo.Save(ctx, fresh); err != nil {
		return nil, fmt.Errorf("save customer: %w", err)
	}
	s.metrics.CustomerLookup(LookupRefreshed)
	return fresh, nil
}
