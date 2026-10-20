CREATE TABLE IF NOT EXISTS payments (
    id             UUID PRIMARY KEY,
    -- One payment per order: the constraint protects against charging an order twice
    -- when the same order.created event is delivered more than once.
    order_id       UUID        NOT NULL UNIQUE,
    customer_id    UUID        NOT NULL,
    amount         BIGINT      NOT NULL CHECK (amount > 0), -- in minor currency units (kopecks/cents)
    currency       CHAR(3)     NOT NULL,
    status         TEXT        NOT NULL,
    failure_reason TEXT,
    transaction_id TEXT,                                    -- provider's transaction ID
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Same structure as order_outbox: see pkg/outbox.
CREATE TABLE IF NOT EXISTS payment_outbox (
    id             UUID PRIMARY KEY,
    aggregate_type TEXT        NOT NULL,
    aggregate_id   TEXT        NOT NULL,
    event_type     TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    attempts       INT         NOT NULL DEFAULT 0,
    last_error     TEXT
);

CREATE INDEX IF NOT EXISTS idx_payment_outbox_unpublished
    ON payment_outbox (created_at, id) WHERE published_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_payment_outbox_published_at
    ON payment_outbox (published_at) WHERE published_at IS NOT NULL;
