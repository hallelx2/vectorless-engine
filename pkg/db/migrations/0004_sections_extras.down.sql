DROP INDEX IF EXISTS sections_doc_pages_idx;
ALTER TABLE sections
    DROP COLUMN IF EXISTS candidate_questions,
    DROP COLUMN IF EXISTS page_end,
    DROP COLUMN IF EXISTS page_start;
