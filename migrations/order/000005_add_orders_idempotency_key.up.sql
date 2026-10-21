-- Idempotency-Key of the request that created the order. The API gateway deduplicates
-- requests in Redis; this constraint is the source of truth when Redis is unavailable
-- or its entry has expired. NULLs (requests without a key) do not conflict.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
-- Hash of the request body: the same key with a different body is a client error.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS request_hash TEXT;
ALTER TABLE orders ADD CONSTRAINT orders_idempotency_key_key UNIQUE (idempotency_key);
