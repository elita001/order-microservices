CREATE TABLE IF NOT EXISTS orders (
    id           UUID PRIMARY KEY,
    customer_id  UUID        NOT NULL,
    status       TEXT        NOT NULL,
    total_amount BIGINT      NOT NULL CHECK (total_amount >= 0), -- in minor currency units (kopecks/cents)
    currency     CHAR(3)     NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_orders_customer_id ON orders (customer_id);

CREATE TABLE IF NOT EXISTS order_items (
    id         BIGSERIAL PRIMARY KEY,
    order_id   UUID   NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    product_id TEXT   NOT NULL,
    quantity   INT    NOT NULL CHECK (quantity > 0),
    price      BIGINT NOT NULL CHECK (price >= 0)
);

CREATE INDEX IF NOT EXISTS idx_order_items_order_id ON order_items (order_id);
