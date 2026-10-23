-- With authentication, idempotency keys are chosen by each customer independently:
-- two customers may use the same key, and one must never get the other's order.
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_idempotency_key_key;
ALTER TABLE orders ADD CONSTRAINT orders_customer_idempotency_key_key UNIQUE (customer_id, idempotency_key);
