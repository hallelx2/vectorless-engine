DROP INDEX IF EXISTS documents_org_store_created_at_idx;
ALTER TABLE documents DROP COLUMN IF EXISTS store_id;
