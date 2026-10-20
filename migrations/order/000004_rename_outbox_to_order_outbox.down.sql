ALTER INDEX IF EXISTS idx_order_outbox_published_at RENAME TO idx_outbox_published_at;
ALTER INDEX IF EXISTS idx_order_outbox_unpublished RENAME TO idx_outbox_unpublished;
ALTER TABLE IF EXISTS order_outbox RENAME CONSTRAINT order_outbox_pkey TO outbox_pkey;
ALTER TABLE IF EXISTS order_outbox RENAME TO outbox;
