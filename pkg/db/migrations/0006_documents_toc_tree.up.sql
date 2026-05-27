-- 0006_documents_toc_tree.up.sql — LLM-built table-of-contents tree.
--
-- PR-A of the PageIndex-style redesign. The ingest pipeline runs an
-- LLM-driven TOC builder on PDFs (between summarize and StatusReady)
-- and persists the result here. The tree is small (a few KB even for
-- 300-page filings) and is read back at retrieval time by strategies
-- that want a hierarchical map of the document independent of the
-- parser's heading detection.
--
-- toc_tree
--     JSONB blob carrying []tree.TOCNode. NULL for documents ingested
--     before this migration, for non-PDF inputs, or when the TOC
--     builder failed (failures are non-fatal — the document remains
--     fully retrievable via the existing sections tree).
--
-- Not indexed: JSONB queries on this column aren't on the hot path.
-- Reads load the blob inline alongside the document row.
ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS toc_tree JSONB;
