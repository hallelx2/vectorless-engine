-- 0004_sections_extras.up.sql — page citations + HyDE candidate questions.
--
-- Two retrieval-quality extensions to the sections table:
--
--  page_start / page_end
--      The inclusive page range each section covers, for parsers that
--      produce page-aware output (PDF today; others leave them NULL/0).
--      Surfaced to API responses so callers can render citations.
--
--  candidate_questions
--      JSONB array of generated questions a section can answer (HyDE).
--      Filled by the ingest pipeline's HyDE stage and woven into the
--      retrieval prompt to widen lexical/semantic overlap with the user
--      query.

ALTER TABLE sections
    ADD COLUMN IF NOT EXISTS page_start          INTEGER,
    ADD COLUMN IF NOT EXISTS page_end            INTEGER,
    ADD COLUMN IF NOT EXISTS candidate_questions JSONB;

CREATE INDEX IF NOT EXISTS sections_doc_pages_idx
    ON sections (document_id, page_start, page_end);
