-- 0005_sections_summary_axes.up.sql — multi-axis structured summaries.
--
-- Phase 2.5 of the retrieval-quality roadmap: the summarizer now returns a
-- structured object ({topics, entities, numbers, one_line}) instead of a
-- single sentence so retrieval has richer signal per section. The existing
-- `summary` column continues to hold the one-line sentence (axes.one_line)
-- for backward compatibility — older SDKs / responses that only read
-- `summary` are unaffected.
--
-- summary_axes
--     JSONB blob carrying the structured shape. NULL for older sections
--     written before this migration; the retrieval-side rendering of axes
--     simply skips when the column is nil, so no backfill is required.
--
-- Not indexed: JSONB queries on this column aren't on the hot path. The
-- retrieval prompt loads sections by document_id (already indexed) and
-- reads axes inline.

ALTER TABLE sections
    ADD COLUMN IF NOT EXISTS summary_axes JSONB;
