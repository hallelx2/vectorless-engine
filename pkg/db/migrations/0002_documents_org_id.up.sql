-- 0002_documents_org_id.up.sql — multi-tenant scoping.
--
-- The engine was originally single-tenant: every caller saw every
-- document. Adding org_id on documents (and reading it on every
-- query) gates each org's data behind their X-Vectorless-Org header.
--
-- Sections inherit scoping transitively via the documents.id FK — we
-- still join through documents on reads so cross-org section IDs
-- can't be enumerated.
--
-- Existing rows are pre-deployment test data. We tag them with the
-- nil UUID so no real user ever surfaces them. A later migration can
-- delete them once we're sure nothing depends on them.

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';

-- Hot path: list documents within an org, newest first.
CREATE INDEX IF NOT EXISTS documents_org_id_created_at_idx
    ON documents (org_id, created_at DESC);
