package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hallelx2/vectorless-engine/internal/tree"
)

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
