CREATE TABLE IF NOT EXISTS notifications (
    id          UUID PRIMARY KEY,
    kind        TEXT        NOT NULL,
    order_id    UUID        NOT NULL,
    customer_id UUID        NOT NULL,
    recipient   TEXT        NOT NULL,
    subject     TEXT        NOT NULL,
    sent_at     TIMESTAMPTZ NOT NULL,
    -- One email of each kind per order: protects customers from duplicate emails
    -- when the same event is delivered more than once.
    CONSTRAINT notifications_kind_order_id_key UNIQUE (kind, order_id)
);

CREATE INDEX IF NOT EXISTS idx_notifications_customer_id ON notifications (customer_id, sent_at DESC);
