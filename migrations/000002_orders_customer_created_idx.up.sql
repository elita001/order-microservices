CREATE INDEX IF NOT EXISTS idx_orders_customer_created
    ON orders (customer_id, created_at DESC, id DESC);
DROP INDEX IF EXISTS idx_orders_customer_id;
