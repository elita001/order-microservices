package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Publisher sends a message to the broker and returns only after the broker has accepted it.
type Publisher interface {
	Publish(ctx context.Context, msg Message) error
}

// RelayConfig is usually loaded from environment variables with the OUTBOX_ prefix.
type RelayConfig struct {
	BatchSize       int           `env:"BATCH_SIZE" envDefault:"100"`     // messages locked and published per transaction
	PollInterval    time.Duration `env:"POLL_INTERVAL" envDefault:"1s"`   // pause between polls when the outbox is empty
	MaxBackoff      time.Duration `env:"MAX_BACKOFF" envDefault:"30s"`    // upper bound for the pause after failures
	PublishTimeout  time.Duration `env:"PUBLISH_TIMEOUT" envDefault:"5s"` // per message, including the broker confirm
	BatchTimeout    time.Duration `env:"BATCH_TIMEOUT" envDefault:"8s"`   // a started batch is finished even during shutdown
	Retention       time.Duration `env:"RETENTION" envDefault:"168h"`     // how long published messages are kept for debugging
	CleanupInterval time.Duration `env:"CLEANUP_INTERVAL" envDefault:"1h"`
}

// Relay moves messages from the outbox table to the broker.
//
// Several replicas can run concurrently: each batch is locked with FOR UPDATE SKIP LOCKED.
// A message is marked as published only after the broker confirms it; if the process
// crashes in between, the message is sent again (at-least-once).
type Relay struct {
	pool      *pgxpool.Pool
	table     string // sanitized identifier, safe to put into queries
	publisher Publisher
	cfg       RelayConfig
	log       *slog.Logger
}

func NewRelay(pool *pgxpool.Pool, table string, publisher Publisher, cfg RelayConfig, log *slog.Logger) *Relay {
	return &Relay{
		pool:      pool,
		table:     pgx.Identifier{table}.Sanitize(),
		publisher: publisher,
		cfg:       cfg,
		log:       log.With("component", "outbox_relay", "table", table),
	}
}

// Run polls the outbox until ctx is canceled. Failures are logged and retried with
// exponential backoff: a broker outage delays events but never blocks order creation.
func (r *Relay) Run(ctx context.Context) error {
	r.log.Info("outbox relay started", "batch_size", r.cfg.BatchSize, "poll_interval", r.cfg.PollInterval.String())

	delay := r.cfg.PollInterval
	var lastCleanup time.Time
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Info("outbox relay stopped")
			return nil
		case <-timer.C:
		}

		if err := r.drain(ctx); err != nil {
			delay = min(delay*2, r.cfg.MaxBackoff)
			r.log.Error("outbox relay failed", "error", err, "retry_in", delay.String())
		} else {
			delay = r.cfg.PollInterval
		}

		if time.Since(lastCleanup) >= r.cfg.CleanupInterval {
			r.cleanup(ctx)
			lastCleanup = time.Now()
		}
		timer.Reset(delay)
	}
}

// drain publishes batches until the outbox is empty, an error occurs, or ctx is canceled.
func (r *Relay) drain(ctx context.Context) error {
	for ctx.Err() == nil {
		n, err := r.publishBatch(ctx)
		if err != nil {
			return err
		}
		if n < r.cfg.BatchSize {
			return nil
		}
	}
	return nil
}

func (r *Relay) publishBatch(ctx context.Context) (int, error) {
	// Finish a started batch even during shutdown: otherwise messages already accepted
	// by the broker would not be marked as published and would be sent again.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.BatchTimeout)
	defer cancel()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT id, aggregate_type, aggregate_id, event_type, payload, created_at
		FROM %s
		WHERE published_at IS NULL
		ORDER BY created_at, id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, r.table), r.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("select messages: %w", err)
	}
	msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Message, error) {
		var msg Message
		err := row.Scan(&msg.ID, &msg.AggregateType, &msg.AggregateID, &msg.EventType, &msg.Payload, &msg.CreatedAt)
		return msg, err
	})
	if err != nil {
		return 0, fmt.Errorf("scan messages: %w", err)
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	published := make([]uuid.UUID, 0, len(msgs))
	var publishErr error
	for _, msg := range msgs {
		pubCtx, cancelPub := context.WithTimeout(ctx, r.cfg.PublishTimeout)
		err := r.publisher.Publish(pubCtx, msg)
		cancelPub()
		if err != nil {
			// Stop at the first failure: the broker is most likely unavailable,
			// and continuing would also break the order of events.
			publishErr = fmt.Errorf("publish message %s (%s): %w", msg.ID, msg.EventType, err)
			if _, err := tx.Exec(ctx, fmt.Sprintf(`
				UPDATE %s SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, r.table),
				msg.ID, publishErr.Error()); err != nil {
				return 0, errors.Join(publishErr, fmt.Errorf("record failure: %w", err))
			}
			break
		}
		published = append(published, msg.ID)
	}

	if len(published) > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET published_at = now() WHERE id = ANY($1)`, r.table), published); err != nil {
			return 0, errors.Join(publishErr, fmt.Errorf("mark published: %w", err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, errors.Join(publishErr, fmt.Errorf("commit: %w", err))
	}
	if len(published) > 0 {
		r.log.Debug("outbox messages published", "count", len(published))
	}
	return len(published), publishErr
}

// cleanup deletes published messages older than the retention period in small chunks,
// so it never holds long locks on the table.
func (r *Relay) cleanup(ctx context.Context) {
	const chunk = 1000
	cutoff := time.Now().Add(-r.cfg.Retention)
	var total int64
	for ctx.Err() == nil {
		tag, err := r.pool.Exec(ctx, fmt.Sprintf(`
			DELETE FROM %[1]s WHERE id IN (
				SELECT id FROM %[1]s WHERE published_at < $1 LIMIT $2
			)`, r.table), cutoff, chunk)
		if err != nil {
			r.log.Error("outbox cleanup failed", "error", err)
			return
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < chunk {
			break
		}
	}
	if total > 0 {
		r.log.Info("outbox cleanup finished", "deleted", total)
	}
}
