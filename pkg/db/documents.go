package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

func itoa(n int) string { return strconv.Itoa(n) }

// DocumentStatus enumerates the ingest lifecycle.
type DocumentStatus string

const (
	StatusPending     DocumentStatus = "pending"
	StatusParsing     DocumentStatus = "parsing"
	StatusSummarizing DocumentStatus = "summarizing"
	StatusReady       DocumentStatus = "ready"
	StatusFailed      DocumentStatus = "failed"
)

// Document is the row-shape for the documents table.
type Document struct {
	ID           tree.DocumentID
	OrgID        string // tenant scope — set on insert, filtered on every read
	Title        string
	ContentType  string
	SourceRef    string
	Status       DocumentStatus
	ErrorMessage string
	ByteSize     int64
	Metadata     map[string]string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewDocument inserts a fresh document row in the "pending" state.
// Caller must populate d.OrgID; the engine is multi-tenant and
// org_id is required on every insert (the DB default exists only for
// pre-migration backfill).
func (p *Pool) NewDocument(ctx context.Context, d Document) error {
	meta, err := marshalMeta(d.Metadata)
	if err != nil {
		return err
	}
	if d.Status == "" {
		d.Status = StatusPending
	}
	if d.OrgID == "" {
		return fmt.Errorf("NewDocument: OrgID is required")
	}
	_, err = p.Exec(ctx, `
        INSERT INTO documents (id, org_id, title, content_type, source_ref, status, byte_size, metadata)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		string(d.ID), d.OrgID, d.Title, d.ContentType, d.SourceRef, string(d.Status), d.ByteSize, meta,
	)
	return mapErr(err)
}

// GetDocument fetches a document by id, scoped to an org. Cross-org
// lookups return ErrNotFound rather than the actual row, so document
// IDs from another tenant can't be confirmed by probing this endpoint.
//
// orgID == "" intentionally panics in callers via the empty-string
// check on the SQL side: it ensures we never accidentally call without
// an org context.
func (p *Pool) GetDocument(ctx context.Context, id tree.DocumentID, orgID string) (*Document, error) {
	if orgID == "" {
		return nil, fmt.Errorf("GetDocument: orgID is required")
	}
	row := p.QueryRow(ctx, `
        SELECT id, org_id, title, content_type, source_ref, status, error_message,
               byte_size, metadata, created_at, updated_at
        FROM documents WHERE id = $1 AND org_id = $2`, string(id), orgID)

	var d Document
	var status string
	var rawMeta []byte
	if err := row.Scan(&d.ID, &d.OrgID, &d.Title, &d.ContentType, &d.SourceRef, &status,
		&d.ErrorMessage, &d.ByteSize, &rawMeta, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	d.Status = DocumentStatus(status)
	d.Metadata = unmarshalMeta(rawMeta)
	return &d, nil
}

// GetDocumentForWorker is the un-scoped variant — used ONLY by the
// background ingest pipeline which is identified by its QStash
// signature rather than an X-Vectorless-Org header. Do NOT call this
// from any user-facing path.
func (p *Pool) GetDocumentForWorker(ctx context.Context, id tree.DocumentID) (*Document, error) {
	row := p.QueryRow(ctx, `
        SELECT id, org_id, title, content_type, source_ref, status, error_message,
               byte_size, metadata, created_at, updated_at
        FROM documents WHERE id = $1`, string(id))

	var d Document
	var status string
	var rawMeta []byte
	if err := row.Scan(&d.ID, &d.OrgID, &d.Title, &d.ContentType, &d.SourceRef, &status,
		&d.ErrorMessage, &d.ByteSize, &rawMeta, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	d.Status = DocumentStatus(status)
	d.Metadata = unmarshalMeta(rawMeta)
	return &d, nil
}

// SetDocumentStatus transitions a document's status and optionally its error.
func (p *Pool) SetDocumentStatus(ctx context.Context, id tree.DocumentID, s DocumentStatus, errMsg string) error {
	_, err := p.Exec(ctx, `
        UPDATE documents
        SET status = $2, error_message = $3, updated_at = now()
        WHERE id = $1`, string(id), string(s), errMsg)
	return mapErr(err)
}

// SetDocumentTitle updates the discovered title after parsing.
func (p *Pool) SetDocumentTitle(ctx context.Context, id tree.DocumentID, title string) error {
	_, err := p.Exec(ctx, `
        UPDATE documents SET title = $2, updated_at = now() WHERE id = $1`,
		string(id), title)
	return mapErr(err)
}

// ListDocumentsOpts controls pagination + filtering for ListDocuments.
type ListDocumentsOpts struct {
	// OrgID restricts the listing to a single tenant. Required.
	OrgID string
	// Limit bounds the page size. Values <= 0 default to 50, capped at 200.
	Limit int
	// Cursor is the last-seen created_at timestamp for keyset pagination.
	// Zero value means "start from newest".
	Cursor time.Time
	// Status, if non-empty, filters by lifecycle status.
	Status DocumentStatus
}

// ListDocuments returns documents in an org, ordered by created_at DESC.
// Use the returned NextCursor (non-zero when more pages exist) to paginate.
func (p *Pool) ListDocuments(ctx context.Context, o ListDocumentsOpts) ([]Document, time.Time, error) {
	if o.OrgID == "" {
		return nil, time.Time{}, fmt.Errorf("ListDocuments: OrgID is required")
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	// Build the WHERE clause dynamically so unused filters don't cost us
	// a planning pass per request.
	where := "WHERE org_id = $1"
	args := []any{o.OrgID}
	next := 2
	if !o.Cursor.IsZero() {
		where += " AND created_at < $" + itoa(next)
		args = append(args, o.Cursor)
		next++
	}
	if o.Status != "" {
		where += " AND status = $" + itoa(next)
		args = append(args, string(o.Status))
		next++
	}
	args = append(args, limit+1) // +1 to detect "has more"

	q := `
        SELECT id, org_id, title, content_type, source_ref, status, error_message,
               byte_size, metadata, created_at, updated_at
        FROM documents ` + where + `
        ORDER BY created_at DESC
        LIMIT $` + itoa(next)

	rows, err := p.Query(ctx, q, args...)
	if err != nil {
		return nil, time.Time{}, mapErr(err)
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		var d Document
		var status string
		var rawMeta []byte
		if err := rows.Scan(&d.ID, &d.OrgID, &d.Title, &d.ContentType, &d.SourceRef, &status,
			&d.ErrorMessage, &d.ByteSize, &rawMeta, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, time.Time{}, err
		}
		d.Status = DocumentStatus(status)
		d.Metadata = unmarshalMeta(rawMeta)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}

	var nextCursor time.Time
	if len(out) > limit {
		nextCursor = out[limit-1].CreatedAt
		out = out[:limit]
	}
	return out, nextCursor, nil
}

// DeleteDocument removes a document (and cascades to its sections),
// scoped to an org. Cross-org deletes return ErrNotFound.
func (p *Pool) DeleteDocument(ctx context.Context, id tree.DocumentID, orgID string) error {
	if orgID == "" {
		return fmt.Errorf("DeleteDocument: orgID is required")
	}
	tag, err := p.Exec(ctx, `DELETE FROM documents WHERE id = $1 AND org_id = $2`, string(id), orgID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func marshalMeta(m map[string]string) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

func unmarshalMeta(raw []byte) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	out := map[string]string{}
	_ = json.Unmarshal(raw, &out)
	return out
}
