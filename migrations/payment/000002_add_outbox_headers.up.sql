-- Context of the operation that produced the event: the W3C trace context (traceparent)
-- and the request ID. The relay puts them into the message headers, so the consumers
-- continue the same trace and log with the same request_id.
ALTER TABLE payment_outbox ADD COLUMN IF NOT EXISTS headers JSONB NOT NULL DEFAULT '{}'::jsonb;
