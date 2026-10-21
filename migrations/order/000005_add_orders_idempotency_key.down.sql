ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_idempotency_key_key;
ALTER TABLE orders DROP COLUMN IF EXISTS request_hash;
ALTER TABLE orders DROP COLUMN IF EXISTS idempotency_key;
