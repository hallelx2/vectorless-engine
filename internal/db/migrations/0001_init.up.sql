-- 0001_init.up.sql — initial schema for vectorless-engine.
--
-- Two first-class entities: documents and sections. Sections form a tree
-- via parent_id (self-referential). Full section content lives in object
-- storage; this table holds only the outline + summaries that the LLM
-- reasons over at query time.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Document status lifecycle:
--   pending     — row created, bytes stored, job enqueued
--   parsing     — worker is parsing the source
--   summarizing — worker is generating section summaries
--   ready       — tree complete, query-able
--   failed      — terminal failure; error_message is populated
CREATE TABLE IF NOT EXISTS documents (
    id              TEXT PRIMARY KEY,
    title           TEXT NOT NULL DEFAULT '',
    content_type    TEXT NOT NULL DEFAULT '',
    source_ref      TEXT NOT NULL,                  -- storage key of original bytes
    status          TEXT NOT NULL DEFAULT 'pending',
    error_message   TEXT NOT NULL DEFAULT '',
    byte_size       BIGINT NOT NULL DEFAULT 0,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS documents_status_idx ON documents (status);
CREATE INDEX IF NOT EXISTS documents_created_at_idx ON documents (created_at DESC);

CREATE TABLE IF NOT EXISTS sections (
    id              TEXT PRIMARY KEY,
    document_id     TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    parent_id       TEXT REFERENCES sections(id) ON DELETE CASCADE,
    ordinal         INTEGER NOT NULL DEFAULT 0,
    depth           INTEGER NOT NULL DEFAULT 0,
    title           TEXT NOT NULL DEFAULT '',
    summary         TEXT NOT NULL DEFAULT '',
    content_ref     TEXT NOT NULL DEFAULT '',       -- storage key for full text
    token_count     INTEGER NOT NULL DEFAULT 0,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sections_document_id_idx    ON sections (document_id);
CREATE INDEX IF NOT EXISTS sections_parent_id_idx      ON sections (parent_id);
CREATE INDEX IF NOT EXISTS sections_document_ordinal_idx ON sections (document_id, parent_id, ordinal);
