package db

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/hallelx2/vectorless-engine/internal/tree"
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
func (p *Pool) NewDocument(ctx context.Context, d Document) error {
	meta, err := marshalMeta(d.Metadata)
	if err != nil {
		return err
	}
	if d.Status == "" {
		d.Status = StatusPending
	}
	_, err = p.Exec(ctx, `
        INSERT INTO documents (id, title, content_type, source_ref, status, byte_size, metadata)
        VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		string(d.ID), d.Title, d.ContentType, d.SourceRef, string(d.Status), d.ByteSize, meta,
	)
	return mapErr(err)
}

// GetDocument fetches a document by id.
func (p *Pool) GetDocument(ctx context.Context, id tree.DocumentID) (*Document, error) {
	row := p.QueryRow(ctx, `
        SELECT id, title, content_type, source_ref, status, error_message,
               byte_size, metadata, created_at, updated_at
        FROM documents WHERE id = $1`, string(id))

	var d Document
	var status string
	var rawMeta []byte
	if err := row.Scan(&d.ID, &d.Title, &d.ContentType, &d.SourceRef, &status,
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
	// Limit bounds the page size. Values <= 0 default to 50, capped at 200.
	Limit int
	// Cursor is the last-seen created_at timestamp for keyset pagination.
	// Zero value means "start from newest".
	Cursor time.Time
	// Status, if non-empty, filters by lifecycle status.
	Status DocumentStatus
}

// ListDocuments returns documents ordered by created_at DESC. Use the
// returned NextCursor (non-zero when more pages exist) to paginate.
func (p *Pool) ListDocuments(ctx context.Context, o ListDocumentsOpts) ([]Document, time.Time, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	// Build the WHERE clause dynamically so unused filters don't cost us
	// a planning pass per request.
	where := "WHERE 1=1"
	args := []any{}
	next := 1
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
        SELECT id, title, content_type, source_ref, status, error_message,
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
		if err := rows.Scan(&d.ID, &d.Title, &d.ContentType, &d.SourceRef, &status,
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

// DeleteDocument removes a document and (via cascade) its sections.
func (p *Pool) DeleteDocument(ctx context.Context, id tree.DocumentID) error {
	tag, err := p.Exec(ctx, `DELETE FROM documents WHERE id = $1`, string(id))
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
