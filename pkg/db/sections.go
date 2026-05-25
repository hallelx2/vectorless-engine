package db

import (
	"context"
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
	Metadata   map[string]string
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
	_, err = p.Exec(ctx, `
        INSERT INTO sections
            (id, document_id, parent_id, ordinal, depth, title, summary,
             content_ref, token_count, metadata)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
        ON CONFLICT (id) DO UPDATE SET
            parent_id   = EXCLUDED.parent_id,
            ordinal     = EXCLUDED.ordinal,
            depth       = EXCLUDED.depth,
            title       = EXCLUDED.title,
            summary     = EXCLUDED.summary,
            content_ref = EXCLUDED.content_ref,
            token_count = EXCLUDED.token_count,
            metadata    = EXCLUDED.metadata,
            updated_at  = now()`,
		string(s.ID), string(s.DocumentID), parent, s.Ordinal, s.Depth,
		s.Title, s.Summary, s.ContentRef, s.TokenCount, meta,
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
               s.title, s.summary, s.content_ref, s.token_count, s.metadata
        FROM sections s
        JOIN documents d ON d.id = s.document_id
        WHERE s.id = $1 AND d.org_id = $2`
	args := []any{string(id), orgID}
	if storeID != "" {
		q += " AND d.store_id = $3"
		args = append(args, storeID)
	}
	row := p.QueryRow(ctx, q, args...)
	var s Section
	var rawMeta []byte
	if err := row.Scan(&s.ID, &s.DocumentID, &s.ParentID, &s.Ordinal, &s.Depth,
		&s.Title, &s.Summary, &s.ContentRef, &s.TokenCount, &rawMeta); err != nil {
		return nil, mapErr(err)
	}
	s.Metadata = unmarshalMeta(rawMeta)
	return &s, nil
}

// GetSectionForWorker is the un-scoped variant — ONLY for the ingest
// pipeline / background workers that have already authenticated via
// QStash signature. Do NOT call from user-facing paths.
func (p *Pool) GetSectionForWorker(ctx context.Context, id tree.SectionID) (*Section, error) {
	row := p.QueryRow(ctx, `
        SELECT id, document_id, COALESCE(parent_id, ''), ordinal, depth,
               title, summary, content_ref, token_count, metadata
        FROM sections WHERE id = $1`, string(id))
	var s Section
	var rawMeta []byte
	if err := row.Scan(&s.ID, &s.DocumentID, &s.ParentID, &s.Ordinal, &s.Depth,
		&s.Title, &s.Summary, &s.ContentRef, &s.TokenCount, &rawMeta); err != nil {
		return nil, mapErr(err)
	}
	s.Metadata = unmarshalMeta(rawMeta)
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
               s.title, s.summary, s.content_ref, s.token_count, s.metadata
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
		var s Section
		var rawMeta []byte
		if err := rows.Scan(&s.ID, &s.DocumentID, &s.ParentID, &s.Ordinal, &s.Depth,
			&s.Title, &s.Summary, &s.ContentRef, &s.TokenCount, &rawMeta); err != nil {
			return nil, err
		}
		s.Metadata = unmarshalMeta(rawMeta)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListSectionsForWorker is the un-scoped variant for background
// workers (LoadTree etc.) that have already authenticated via QStash.
func (p *Pool) ListSectionsForWorker(ctx context.Context, docID tree.DocumentID) ([]Section, error) {
	rows, err := p.Query(ctx, `
        SELECT id, document_id, COALESCE(parent_id, ''), ordinal, depth,
               title, summary, content_ref, token_count, metadata
        FROM sections
        WHERE document_id = $1
        ORDER BY depth ASC, ordinal ASC`, string(docID))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []Section
	for rows.Next() {
		var s Section
		var rawMeta []byte
		if err := rows.Scan(&s.ID, &s.DocumentID, &s.ParentID, &s.Ordinal, &s.Depth,
			&s.Title, &s.Summary, &s.ContentRef, &s.TokenCount, &rawMeta); err != nil {
			return nil, err
		}
		s.Metadata = unmarshalMeta(rawMeta)
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
			ID:         r.ID,
			ParentID:   r.ParentID,
			Ordinal:    r.Ordinal,
			Title:      r.Title,
			Summary:    r.Summary,
			ContentRef: r.ContentRef,
			TokenCount: r.TokenCount,
			Metadata:   r.Metadata,
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
