// Package postgres implements the notification persistence adapter.
package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elita001/order-microservices/internal/notification/domain"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// SendOnce inserts the notification and calls send in one transaction.
//
// The unique (kind, order_id) index makes concurrent duplicates wait: the second INSERT
// blocks until the first transaction finishes and then hits the conflict, so the email
// is sent once. If send fails, the transaction is rolled back and a retry sends again.
// The transaction stays open while the email is sent, which is fine for a short SMTP call
// with a timeout.
func (r *Repository) SendOnce(ctx context.Context, n *domain.Notification, send func(ctx context.Context) error) (bool, error) {
	var sent bool
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO notifications (id, kind, order_id, customer_id, recipient, subject, sent_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (kind, order_id) DO NOTHING`,
			n.ID, string(n.Kind), n.OrderID, n.CustomerID, n.Recipient, n.Subject, n.SentAt)
		if err != nil {
			return fmt.Errorf("insert notification: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if err := send(ctx); err != nil {
			return err
		}
		sent = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return sent, nil
}

func (r *Repository) Sent(ctx context.Context, kind domain.Kind, orderID uuid.UUID) (bool, error) {
	var sent bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM notifications WHERE kind = $1 AND order_id = $2)`,
		string(kind), orderID).Scan(&sent)
	if err != nil {
		return false, fmt.Errorf("check notification: %w", err)
	}
	return sent, nil
}
