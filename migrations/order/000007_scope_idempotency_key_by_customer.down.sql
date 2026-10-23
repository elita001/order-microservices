ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_customer_idempotency_key_key;
ALTER TABLE orders ADD CONSTRAINT orders_idempotency_key_key UNIQUE (idempotency_key);
