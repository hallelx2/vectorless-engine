-- 0007_documents_idempotency_key.up.sql — idempotent ingestion.
--
-- The SDK already sends an optional `Idempotency-Key` header on ingest
-- ("Prevents duplicate ingestion"), but the engine ignored it: a client
-- (or its built-in transport retry) that re-POSTs the same upload after a
-- transient network error created a SECOND document with a fresh id, a
-- fresh source object, and a fresh parse job. Under heavy concurrent
-- bulk ingestion on a flaky link this produced the same corpus document
-- ingested up to 6× (HAL-323).
--
-- Honoring the key requires a place to store it and a uniqueness
-- guarantee scoped per tenant. A PARTIAL unique index lets the column be
-- NULL for callers that don't supply a key (no dedup, today's behavior)
-- while making (org_id, idempotency_key) collide-and-return for callers
-- that do.

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS documents_org_idempotency_key_uidx
    ON documents (org_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
