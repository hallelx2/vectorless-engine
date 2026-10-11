-- 0008_documents_progress.up.sql — live ingest progress.
--
-- status says where a document is in one word; progress says what each
-- stage of ingest is doing and how long it took: an ordered JSON array of
-- {name, label, state, started_at, ended_at, detail}. The ingest worker is
-- the only writer and rewrites the array whole on each change. The
-- dashboard polls it to draw the pipeline while a document processes.
ALTER TABLE documents ADD COLUMN IF NOT EXISTS progress JSONB NOT NULL DEFAULT '[]'::jsonb;
