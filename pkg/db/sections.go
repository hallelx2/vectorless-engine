package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Section is the row-shape for the sections table.
type Section struct {
	ID         tree.SectionID
	DocumentID tree.DocumentID
	ParentID   tree.SectionID // empty for root
	Ordinal    int
	Depth      int
	Title      string
	Summary    string
	ContentRef string
	TokenCount int

	// PageStart / PageEnd is the inclusive page range this section
	// covers, when known. Zero means "unknown" (NULL in DB) and is the
	// expected value for non-paginated formats (Markdown, HTML, DOCX,
	// text). The PDF parser populates them.
	PageStart int
	PageEnd   int

	// CandidateQuestions is the list of HyDE-generated questions this
	// section can answer. Persisted as JSONB; nil means "not yet
	// generated".
	CandidateQuestions []string

	// SummaryAxes is the Phase 2.5 multi-axis structured summary
	// (topics / entities / numbers / one_line). Persisted as JSONB;
	// nil means "not yet generated" — older sections written before
	// 0005_sections_summary_axes carry NULL and are still query-able
	// via the plain Summary field.
	SummaryAxes *tree.SummaryAxes

	Metadata map[string]string
}

// sectionSelectColumns is the canonical SELECT list for fetching section
// rows — kept in one place so adding a column doesn't drift across the
// scoped / worker / list variants.
const sectionSelectColumns = `id, document_id, COALESCE(parent_id, ''), ordinal, depth,
               title, summary, content_ref, token_count, metadata,
               page_start, page_end, candidate_questions, summary_axes`

// scanSectionRow scans columns in the same order as sectionSelectColumns.
// Used by every section-fetching method to keep parsing in lockstep with
// the column list above.
func scanSectionRow(row interface {
	Scan(dest ...any) error
}) (Section, error) {
	var s Section
	var rawMeta, rawCandidates, rawAxes []byte
	var pageStart, pageEnd sql.NullInt64
	if err := row.Scan(&s.ID, &s.DocumentID, &s.ParentID, &s.Ordinal, &s.Depth,
		&s.Title, &s.Summary, &s.ContentRef, &s.TokenCount, &rawMeta,
		&pageStart, &pageEnd, &rawCandidates, &rawAxes); err != nil {
		return s, err
	}
	s.Metadata = unmarshalMeta(rawMeta)
	s.PageStart = scanNullableInt(pageStart)
	s.PageEnd = scanNullableInt(pageEnd)
	s.CandidateQuestions = unmarshalCandidateQuestions(rawCandidates)
	s.SummaryAxes = unmarshalSummaryAxes(rawAxes)
	return s, nil
}

// UpsertSection inserts or updates a section row. Callers should insert in
// tree order (parents before children) so the ParentID FK is satisfied.
func (p *Pool) UpsertSection(ctx context.Context, s Section) error {
	meta, err := marshalMeta(s.Metadata)
	if err != nil {
		return err
	}
	var parent any
	if s.ParentID != "" {
		parent = string(s.ParentID)
	}
	pageStart := nullIfZero(s.PageStart)
	pageEnd := nullIfZero(s.PageEnd)
	candidates, err := marshalCandidateQuestions(s.CandidateQuestions)
	if err != nil {
		return err
	}
	axes, err := marshalSummaryAxes(s.SummaryAxes)
	if err != nil {
		return err
	}
	_, err = p.Exec(ctx, `
        INSERT INTO sections
            (id, document_id, parent_id, ordinal, depth, title, summary,
             content_ref, token_count, metadata, page_start, page_end,
             candidate_questions, summary_axes)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
        ON CONFLICT (id) DO UPDATE SET
            parent_id           = EXCLUDED.parent_id,
            ordinal             = EXCLUDED.ordinal,
            depth               = EXCLUDED.depth,
            title               = EXCLUDED.title,
            summary             = EXCLUDED.summary,
            content_ref         = EXCLUDED.content_ref,
            token_count         = EXCLUDED.token_count,
            metadata            = EXCLUDED.metadata,
            page_start          = EXCLUDED.page_start,
            page_end            = EXCLUDED.page_end,
            candidate_questions = EXCLUDED.candidate_questions,
            summary_axes        = EXCLUDED.summary_axes,
            updated_at          = now()`,
		string(s.ID), string(s.DocumentID), parent, s.Ordinal, s.Depth,
		s.Title, s.Summary, s.ContentRef, s.TokenCount, meta,
		pageStart, pageEnd, candidates, axes,
	)
	return mapErr(err)
}

// UpdateSectionSummary patches only the summary + token count for a section.
func (p *Pool) UpdateSectionSummary(ctx context.Context, id tree.SectionID, summary string, tokens int) error {
	_, err := p.Exec(ctx, `
        UPDATE sections
        SET summary = $2, token_count = $3, updated_at = now()
        WHERE id = $1`, string(id), summary, tokens)
	return mapErr(err)
}

// UpdateSectionCandidateQuestions persists the HyDE-generated questions
// for a section. Pass nil to clear (stores SQL NULL).
func (p *Pool) UpdateSectionCandidateQuestions(ctx context.Context, id tree.SectionID, questions []string) error {
	candidates, err := marshalCandidateQuestions(questions)
	if err != nil {
		return err
	}
	_, err = p.Exec(ctx, `
        UPDATE sections
        SET candidate_questions = $2, updated_at = now()
        WHERE id = $1`, string(id), candidates)
	return mapErr(err)
}

// UpdateSectionSummaryAxes persists the Phase 2.5 multi-axis summary
// blob for a section. Pass nil to clear (stores SQL NULL — the
// "not yet generated" state). Mirrors UpdateSectionSummary so the two
// fields can be patched independently as the pipeline progresses.
func (p *Pool) UpdateSectionSummaryAxes(ctx context.Context, id tree.SectionID, axes *tree.SummaryAxes) error {
	raw, err := marshalSummaryAxes(axes)
	if err != nil {
		return err
	}
	_, err = p.Exec(ctx, `
        UPDATE sections
        SET summary_axes = $2, updated_at = now()
        WHERE id = $1`, string(id), raw)
	return mapErr(err)
}

// nullIfZero returns SQL NULL when n == 0, otherwise n. Used so unknown
// page ranges land as NULL in DB rather than collapsing to "page 0".
func nullIfZero(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}

// marshalCandidateQuestions encodes a candidate-questions slice as JSONB.
// nil → SQL NULL (the "not yet generated" state). An empty non-nil slice
// → `[]` (explicitly "no questions found"), so callers can distinguish.
func marshalCandidateQuestions(qs []string) (any, error) {
	if qs == nil {
		return nil, nil
	}
	b, err := json.Marshal(qs)
	if err != nil {
		return nil, fmt.Errorf("marshal candidate_questions: %w", err)
	}
	return b, nil
}

// unmarshalCandidateQuestions decodes a JSONB candidate_questions blob.
// NULL / zero-length → nil.
func unmarshalCandidateQuestions(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// marshalSummaryAxes encodes a SummaryAxes pointer as JSONB. nil → SQL
// NULL (the "not yet generated" state). A non-nil pointer is always
// stored as the object form, even when every axis is empty, so callers
// can distinguish "we generated axes and got nothing" from "we haven't
// tried yet". Mirrors marshalCandidateQuestions.
func marshalSummaryAxes(a *tree.SummaryAxes) (any, error) {
	if a == nil {
		return nil, nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("marshal summary_axes: %w", err)
	}
	return b, nil
}

// unmarshalSummaryAxes decodes a JSONB summary_axes blob. NULL /
// zero-length → nil. Garbled bytes degrade to nil rather than panic so
// a stray bad row can't take down listing endpoints.
func unmarshalSummaryAxes(raw []byte) *tree.SummaryAxes {
	if len(raw) == 0 {
		return nil
	}
	var out tree.SummaryAxes
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
}

// scanNullableInt unwraps a sql.NullInt64 into a plain int (0 = NULL).
func scanNullableInt(n sql.NullInt64) int {
	if !n.Valid {
		return 0
	}
	return int(n.Int64)
}

// CountSections returns the number of sections persisted for a
// document, scoped via JOIN on the parent document's org + store.
// storeID == "" skips the store filter.
func (p *Pool) CountSections(ctx context.Context, docID tree.DocumentID, orgID, storeID string) (int, error) {
	if orgID == "" {
		return 0, fmt.Errorf("CountSections: orgID is required")
	}
	q := `
        SELECT count(*)
        FROM sections s
        JOIN documents d ON d.id = s.document_id
        WHERE s.document_id = $1 AND d.org_id = $2`
	args := []any{string(docID), orgID}
	if storeID != "" {
		q += " AND d.store_id = $3"
		args = append(args, storeID)
	}
	var n int
	if err := p.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, mapErr(err)
	}
	return n, nil
}

// GetSection fetches a single section, scoped to an org (and optional
// store) via JOIN on the parent document. Cross-scope reads return
// ErrNotFound, so section IDs from other tenants/stores can't be probed.
func (p *Pool) GetSection(ctx context.Context, id tree.SectionID, orgID, storeID string) (*Section, error) {
	if orgID == "" {
		return nil, fmt.Errorf("GetSection: orgID is required")
	}
	q := `
        SELECT s.id, s.document_id, COALESCE(s.parent_id, ''), s.ordinal, s.depth,
               s.title, s.summary, s.content_ref, s.token_count, s.metadata,
               s.page_start, s.page_end, s.candidate_questions, s.summary_axes
        FROM sections s
        JOIN documents d ON d.id = s.document_id
        WHERE s.id = $1 AND d.org_id = $2`
	args := []any{string(id), orgID}
	if storeID != "" {
		q += " AND d.store_id = $3"
		args = append(args, storeID)
	}
	row := p.QueryRow(ctx, q, args...)
	s, err := scanSectionRow(row)
	if err != nil {
		return nil, mapErr(err)
	}
	return &s, nil
}

// GetSectionForWorker is the un-scoped variant — ONLY for the ingest
// pipeline / background workers that have already authenticated via
// QStash signature. Do NOT call from user-facing paths.
func (p *Pool) GetSectionForWorker(ctx context.Context, id tree.SectionID) (*Section, error) {
	row := p.QueryRow(ctx, `
        SELECT `+sectionSelectColumns+`
        FROM sections WHERE id = $1`, string(id))
	s, err := scanSectionRow(row)
	if err != nil {
		return nil, mapErr(err)
	}
	return &s, nil
}

// ListSections returns every section for a document in tree order
// (parent before children, ordered by ordinal within a parent),
// scoped to an org (and optional store) via JOIN on the parent document.
func (p *Pool) ListSections(ctx context.Context, docID tree.DocumentID, orgID, storeID string) ([]Section, error) {
	if orgID == "" {
		return nil, fmt.Errorf("ListSections: orgID is required")
	}
	q := `
        SELECT s.id, s.document_id, COALESCE(s.parent_id, ''), s.ordinal, s.depth,
               s.title, s.summary, s.content_ref, s.token_count, s.metadata,
               s.page_start, s.page_end, s.candidate_questions, s.summary_axes
        FROM sections s
        JOIN documents d ON d.id = s.document_id
        WHERE s.document_id = $1 AND d.org_id = $2`
	args := []any{string(docID), orgID}
	if storeID != "" {
		q += " AND d.store_id = $3"
		args = append(args, storeID)
	}
	q += " ORDER BY s.depth ASC, s.ordinal ASC"
	rows, err := p.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []Section
	for rows.Next() {
		s, err := scanSectionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListSectionsForWorker is the un-scoped variant for background
// workers (LoadTree etc.) that have already authenticated via QStash.
func (p *Pool) ListSectionsForWorker(ctx context.Context, docID tree.DocumentID) ([]Section, error) {
	rows, err := p.Query(ctx, `
        SELECT `+sectionSelectColumns+`
        FROM sections
        WHERE document_id = $1
        ORDER BY depth ASC, ordinal ASC`, string(docID))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []Section
	for rows.Next() {
		s, err := scanSectionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LoadTree reconstructs a tree.Tree from the documents + sections
// tables, scoped to an org (and optional store). Use LoadTreeForWorker
// from background jobs that don't have a scope but are otherwise trusted.
func (p *Pool) LoadTree(ctx context.Context, docID tree.DocumentID, orgID, storeID string) (*tree.Tree, error) {
	if orgID == "" {
		return nil, fmt.Errorf("LoadTree: orgID is required")
	}
	doc, err := p.GetDocument(ctx, docID, orgID, storeID)
	if err != nil {
		return nil, err
	}
	rows, err := p.ListSections(ctx, docID, orgID, storeID)
	if err != nil {
		return nil, err
	}
	return buildTree(doc, rows), nil
}

// LoadTreeForWorker is the un-scoped variant used by the ingest
// pipeline (which trusts itself) and the retrieval strategy when
// it's called from a worker context.
func (p *Pool) LoadTreeForWorker(ctx context.Context, docID tree.DocumentID) (*tree.Tree, error) {
	doc, err := p.GetDocumentForWorker(ctx, docID)
	if err != nil {
		return nil, err
	}
	rows, err := p.ListSectionsForWorker(ctx, docID)
	if err != nil {
		return nil, err
	}
	return buildTree(doc, rows), nil
}

func buildTree(doc *Document, rows []Section) *tree.Tree {

	byID := make(map[tree.SectionID]*tree.Section, len(rows))
	for i := range rows {
		r := rows[i]
		byID[r.ID] = &tree.Section{
			ID:                 r.ID,
			ParentID:           r.ParentID,
			Ordinal:            r.Ordinal,
			Title:              r.Title,
			Summary:            r.Summary,
			SummaryAxes:        r.SummaryAxes,
			ContentRef:         r.ContentRef,
			TokenCount:         r.TokenCount,
			PageStart:          r.PageStart,
			PageEnd:            r.PageEnd,
			CandidateQuestions: r.CandidateQuestions,
			Metadata:           r.Metadata,
		}
	}

	// Collect every section whose parent_id is empty — these are
	// "top-level" sections. Older code picked just the first one as
	// the tree root and silently dropped the other siblings + their
	// subtrees. Now we wrap them in a synthetic root so callers see
	// the whole document.
	var topLevel []*tree.Section
	for i := range rows {
		s := byID[rows[i].ID]
		if s.ParentID == "" {
			topLevel = append(topLevel, s)
			continue
		}
		parent, ok := byID[s.ParentID]
		if !ok {
			continue // orphan; shouldn't happen given FK
		}
		parent.Children = append(parent.Children, s)
	}

	var root *tree.Section
	switch len(topLevel) {
	case 0:
		root = nil // empty doc
	case 1:
		root = topLevel[0]
	default:
		// Multiple top-level sections — wrap in a synthetic root so
		// BuildView walks all of them. The synthetic root carries the
		// document's title and an empty ID so consumers can distinguish
		// it from a real section.
		root = &tree.Section{
			ID:       "",
			ParentID: "",
			Title:    doc.Title,
			Children: topLevel,
		}
	}

	return &tree.Tree{
		DocumentID: doc.ID,
		Title:      doc.Title,
		Root:       root,
		CreatedAt:  doc.CreatedAt,
		UpdatedAt:  doc.UpdatedAt,
	}
}
