-- Every service owns its tables; the prefix makes the owner explicit
-- now that the payment service has its own outbox.
ALTER TABLE IF EXISTS outbox RENAME TO order_outbox;
ALTER TABLE IF EXISTS order_outbox RENAME CONSTRAINT outbox_pkey TO order_outbox_pkey;
ALTER INDEX IF EXISTS idx_outbox_unpublished RENAME TO idx_order_outbox_unpublished;
ALTER INDEX IF EXISTS idx_outbox_published_at RENAME TO idx_order_outbox_published_at;
