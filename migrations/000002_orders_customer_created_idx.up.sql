CREATE INDEX IF NOT EXISTS idx_orders_customer_id ON orders (customer_id);
DROP INDEX IF EXISTS idx_orders_customer_created;