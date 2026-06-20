-- 0007_documents_idempotency_key.down.sql

DROP INDEX IF EXISTS documents_org_idempotency_key_uidx;

ALTER TABLE documents
    DROP COLUMN IF EXISTS idempotency_key;
