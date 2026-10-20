CREATE TABLE IF NOT EXISTS shipments (
    id                    UUID PRIMARY KEY,
    -- One shipment per order: the constraint protects against registering a second parcel
    -- when the same payment.succeeded event is delivered more than once.
    order_id              UUID        NOT NULL UNIQUE,
    customer_id           UUID        NOT NULL,
    status                TEXT        NOT NULL,
    carrier               TEXT,
    tracking_number       TEXT,
    estimated_delivery_at TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Same structure as order_outbox: see pkg/outbox.
CREATE TABLE IF NOT EXISTS delivery_outbox (
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

CREATE INDEX IF NOT EXISTS idx_delivery_outbox_unpublished
    ON delivery_outbox (created_at, id) WHERE published_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_delivery_outbox_published_at
    ON delivery_outbox (published_at) WHERE published_at IS NOT NULL;
