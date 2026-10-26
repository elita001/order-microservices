// Package postgres implements the customer persistence adapter.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elita001/order-microservices/internal/customer/application"
	"github.com/elita001/order-microservices/internal/customer/domain"
)

type Repository struct {
	pool *pgxpool.Pool
}

var _ application.Repository = (*Repository)(nil)

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	var c domain.Customer
	err := r.pool.QueryRow(ctx, `
		SELECT id, username, email, first_name, last_name, synced_at
		FROM customers WHERE id = $1`, id).
		Scan(&c.ID, &c.Username, &c.Email, &c.FirstName, &c.LastName, &c.SyncedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select customer: %w", err)
	}
	return &c, nil
}

// Save inserts the customer or replaces the copy. Concurrent refreshes of the same customer
// are harmless: the last one wins, and both wrote what the identity provider returned.
func (r *Repository) Save(ctx context.Context, c *domain.Customer) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO customers (id, username, email, first_name, last_name, synced_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			username = EXCLUDED.username, email = EXCLUDED.email,
			first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name,
			synced_at = EXCLUDED.synced_at`,
		c.ID, c.Username, c.Email, c.FirstName, c.LastName, c.SyncedAt)
	if err != nil {
		return fmt.Errorf("upsert customer: %w", err)
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := r.pool.Exec(ctx, `DELETE FROM customers WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete customer: %w", err)
	}
	return nil
}
