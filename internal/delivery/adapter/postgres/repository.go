// Package postgres implements the delivery persistence adapter.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elita001/order-microservices/internal/delivery/domain"
	"github.com/elita001/order-microservices/pkg/outbox"
)

// OutboxTable is the delivery service's outbox table.
const OutboxTable = "delivery_outbox"

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// GetOrCreate inserts the shipment unless the order already has one, then returns the
// stored shipment. These are two statements on purpose: the SELECT gets a fresh snapshot
// and sees a shipment that a concurrent transaction has just committed.
func (r *Repository) GetOrCreate(ctx context.Context, s *domain.Shipment) (*domain.Shipment, error) {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO shipments (id, order_id, customer_id, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (order_id) DO NOTHING`,
		s.ID, s.OrderID, s.CustomerID, string(s.Status), s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert shipment: %w", err)
	}
	return r.getByOrderID(ctx, s.OrderID)
}

// Complete updates a pending shipment and stores its events in the outbox in one transaction.
// The status check in WHERE guarantees that only one of concurrent deliveries publishes events.
func (r *Repository) Complete(ctx context.Context, s *domain.Shipment, domainEvents ...domain.Event) (bool, error) {
	msgs, err := toOutboxMessages(domainEvents)
	if err != nil {
		return false, err
	}

	var completed bool
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE shipments
			SET status = $2, carrier = $3, tracking_number = $4, estimated_delivery_at = $5, updated_at = $6
			WHERE id = $1 AND status = $7`,
			s.ID, string(s.Status), s.Carrier, s.TrackingNumber, s.EstimatedDeliveryAt, s.UpdatedAt,
			string(domain.StatusPending))
		if err != nil {
			return fmt.Errorf("update shipment: %w", err)
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

func (r *Repository) getByOrderID(ctx context.Context, orderID uuid.UUID) (*domain.Shipment, error) {
	var (
		s      domain.Shipment
		status string
		eta    *time.Time
	)
	err := r.pool.QueryRow(ctx, `
		SELECT id, order_id, customer_id, status, COALESCE(carrier, ''), COALESCE(tracking_number, ''),
		       estimated_delivery_at, created_at, updated_at
		FROM shipments WHERE order_id = $1`, orderID).
		Scan(&s.ID, &s.OrderID, &s.CustomerID, &status, &s.Carrier, &s.TrackingNumber,
			&eta, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("select shipment: %w", err)
	}
	s.Status = domain.Status(status)
	if eta != nil {
		s.EstimatedDeliveryAt = *eta
	}
	return &s, nil
}
