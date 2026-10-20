// Package postgres implements the payment persistence adapter.
package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elita001/order-microservices/internal/payment/domain"
	"github.com/elita001/order-microservices/pkg/outbox"
)

// OutboxTable is the payment service's outbox table.
const OutboxTable = "payment_outbox"

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// GetOrCreate inserts the payment unless the order already has one, then returns the
// stored payment. These are two statements on purpose: the SELECT gets a fresh snapshot
// and sees a payment that a concurrent transaction has just committed.
func (r *Repository) GetOrCreate(ctx context.Context, p *domain.Payment) (*domain.Payment, error) {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO payments (id, order_id, customer_id, amount, currency, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (order_id) DO NOTHING`,
		p.ID, p.OrderID, p.CustomerID, p.Amount, p.Currency, string(p.Status), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert payment: %w", err)
	}
	return r.getByOrderID(ctx, p.OrderID)
}

// Complete updates a pending payment and stores its events in the outbox in one transaction.
// The status check in WHERE guarantees that only one of concurrent deliveries publishes events.
func (r *Repository) Complete(ctx context.Context, p *domain.Payment, domainEvents ...domain.Event) (bool, error) {
	msgs, err := toOutboxMessages(domainEvents)
	if err != nil {
		return false, err
	}

	var completed bool
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE payments
			SET status = $2, failure_reason = NULLIF($3, ''), transaction_id = NULLIF($4, ''), updated_at = $5
			WHERE id = $1 AND status = $6`,
			p.ID, string(p.Status), p.FailureReason, p.TransactionID, p.UpdatedAt, string(domain.StatusPending))
		if err != nil {
			return fmt.Errorf("update payment: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		completed = true
		return outbox.Store(ctx, tx, OutboxTable, msgs...)
	})
	if err != nil {
		return false, err
	}
	return completed, nil
}

func (r *Repository) getByOrderID(ctx context.Context, orderID uuid.UUID) (*domain.Payment, error) {
	var (
		p      domain.Payment
		status string
	)
	err := r.pool.QueryRow(ctx, `
		SELECT id, order_id, customer_id, amount, currency, status,
		       COALESCE(failure_reason, ''), COALESCE(transaction_id, ''), created_at, updated_at
		FROM payments WHERE order_id = $1`, orderID).
		Scan(&p.ID, &p.OrderID, &p.CustomerID, &p.Amount, &p.Currency, &status,
			&p.FailureReason, &p.TransactionID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("select payment: %w", err)
	}
	p.Status = domain.Status(status)
	return &p, nil
}
