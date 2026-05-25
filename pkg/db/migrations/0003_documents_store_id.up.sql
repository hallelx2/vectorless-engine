-- 0003_documents_store_id.up.sql — store scoping (Org → Store → Documents).
--
-- A store is a named collection within an org (control-plane entity).
-- The engine only sees store_id as an opaque scoping column, exactly
-- like org_id. Reads filter by it when the X-Vectorless-Store header
-- is present; writes always set it (header value, or the nil sentinel
-- for header-less / pre-stores callers so today's single-pool
-- behavior keeps working).
--
-- Existing rows predate stores; they keep the nil sentinel and remain
-- visible only to header-less requests.

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS store_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';

-- Hot path: list documents within an org's store, newest first.
CREATE INDEX IF NOT EXISTS documents_org_store_created_at_idx
    ON documents (org_id, store_id, created_at DESC);
