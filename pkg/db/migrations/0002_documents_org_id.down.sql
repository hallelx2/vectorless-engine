DROP INDEX IF EXISTS documents_org_id_created_at_idx;
ALTER TABLE documents DROP COLUMN IF EXISTS org_id;
