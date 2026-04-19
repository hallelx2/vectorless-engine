package db

import (
	"context"

	"github.com/hallelx2/vectorless-engine/internal/tree"
)

// Section is the row-shape for the sections table.
type Section struct {
	ID          tree.SectionID
	DocumentID  tree.DocumentID
	ParentID    tree.SectionID // empty for root
	Ordinal     int
	Depth       int
	Title       string
	Summary     string
	ContentRef  string
	TokenCount  int
	Metadata    map[string]string
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

// GetSection fetches a single section.
func (p *Pool) GetSection(ctx context.Context, id tree.SectionID) (*Section, error) {
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
// (parent before children, ordered by ordinal within a parent).
func (p *Pool) ListSections(ctx context.Context, docID tree.DocumentID) ([]Section, error) {
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

// LoadTree reconstructs a tree.Tree from the documents + sections tables.
func (p *Pool) LoadTree(ctx context.Context, docID tree.DocumentID) (*tree.Tree, error) {
	doc, err := p.GetDocument(ctx, docID)
	if err != nil {
		return nil, err
	}
	rows, err := p.ListSections(ctx, docID)
	if err != nil {
		return nil, err
	}

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

	var root *tree.Section
	for i := range rows {
		s := byID[rows[i].ID]
		if s.ParentID == "" {
			if root == nil {
				root = s
			}
			continue
		}
		parent, ok := byID[s.ParentID]
		if !ok {
			continue // orphan; shouldn't happen given FK
		}
		parent.Children = append(parent.Children, s)
	}

	return &tree.Tree{
		DocumentID: doc.ID,
		Title:      doc.Title,
		Root:       root,
		CreatedAt:  doc.CreatedAt,
		UpdatedAt:  doc.UpdatedAt,
	}, nil
}
