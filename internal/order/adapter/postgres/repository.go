// Package postgres implements the order persistence adapter.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elita001/order-microservices/internal/order/domain"
	"github.com/elita001/order-microservices/pkg/outbox"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Create saves the order, its items, and its events to the outbox in one transaction.
func (r *Repository) Create(ctx context.Context, order *domain.Order, domainEvents ...domain.Event) error {
	msgs, err := toOutboxMessages(domainEvents)
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO orders (id, customer_id, status, total_amount, currency, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			order.ID, order.CustomerID, string(order.Status), order.TotalAmount, order.Currency, order.CreatedAt, order.UpdatedAt)
		if err != nil {
			return fmt.Errorf("insert order: %w", err)
		}

		batch := &pgx.Batch{}
		for _, item := range order.Items {
			batch.Queue(`
				INSERT INTO order_items (order_id, product_id, quantity, price)
				VALUES ($1, $2, $3, $4)`,
				order.ID, item.ProductID, item.Quantity, item.Price)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("insert order items: %w", err)
		}
		return outbox.Store(ctx, tx, msgs...)
	})
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Order, error) {
	var (
		order  domain.Order
		status string
	)
	err := r.pool.QueryRow(ctx, `
		SELECT id, customer_id, status, total_amount, currency, created_at, updated_at
		FROM orders WHERE id = $1`, id).
		Scan(&order.ID, &order.CustomerID, &status, &order.TotalAmount, &order.Currency, &order.CreatedAt, &order.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select order: %w", err)
	}
	order.Status = domain.Status(status)

	rows, err := r.pool.Query(ctx, `
		SELECT product_id, quantity, price
		FROM order_items WHERE order_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("select order items: %w", err)
	}
	order.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Item, error) {
		var item domain.Item
		err := row.Scan(&item.ProductID, &item.Quantity, &item.Price)
		return item, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan order items: %w", err)
	}
	return &order, nil
}

func (r *Repository) ListByCustomer(ctx context.Context, customerID uuid.UUID, limit, offset int) ([]domain.Order, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, customer_id, status, total_amount, currency, created_at, updated_at
		FROM orders WHERE customer_id = $1
		ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, customerID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("select orders by customer ID: %w", err)
	}
	orders, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Order, error) {
		var (
			order  domain.Order
			status string
		)
		err := row.Scan(&order.ID, &order.CustomerID, &status, &order.TotalAmount, &order.Currency, &order.CreatedAt, &order.UpdatedAt)
		order.Status = domain.Status(status)
		return order, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan orders: %w", err)
	}
	return orders, rows.Err()
}
