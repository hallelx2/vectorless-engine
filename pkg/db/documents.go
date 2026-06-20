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

// NilScope is the sentinel store_id used for documents ingested
// without a store context (header-less / pre-stores callers). Reads
// with an empty store filter ignore the column entirely; this value
// just keeps the NOT NULL column populated.
const NilScope = "00000000-0000-0000-0000-000000000000"

// Document is the row-shape for the documents table.
type Document struct {
	ID           tree.DocumentID
	OrgID        string // tenant scope — set on insert, filtered on every read
	StoreID      string // collection scope within the org
	Title        string
	ContentType  string
	SourceRef    string
	Status       DocumentStatus
	ErrorMessage string
	ByteSize     int64
	Metadata     map[string]string

	// IdempotencyKey is the optional client-supplied Idempotency-Key.
	// When non-empty it is unique per (org_id, idempotency_key): a repeat
	// ingest with the same key returns the existing document instead of
	// creating a duplicate. Empty means "no dedup" (column stored as NULL).
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time

	// TOCTree is the JSONB blob persisted by the ingest pipeline's
	// LLM-driven TOC builder ([]tree.TOCNode marshalled). nil
	// (NULL in DB) means "not yet generated" — the expected state
	// for non-PDF documents, for documents ingested before the
	// 0006 migration, and when the builder failed (builder
	// failures are non-fatal and leave this column NULL).
	//
	// Stored raw so the column round-trips byte-identically
	// regardless of slice-element ordering inside the encoder.
	// Callers that need the typed shape unmarshal at read time.
	TOCTree []byte
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
	storeID := d.StoreID
	if storeID == "" {
		storeID = NilScope
	}
	// NULL (not "") when no key, so the partial unique index ignores the row.
	var idemKey any
	if d.IdempotencyKey != "" {
		idemKey = d.IdempotencyKey
	}
	_, err = p.Exec(ctx, `
        INSERT INTO documents (id, org_id, store_id, title, content_type, source_ref, status, byte_size, metadata, idempotency_key)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		string(d.ID), d.OrgID, storeID, d.Title, d.ContentType, d.SourceRef, string(d.Status), d.ByteSize, meta, idemKey,
	)
	return mapErr(err)
}

// GetDocumentByIdempotencyKey returns the document an org previously ingested
// under key, or ErrNotFound if none exists. Used to short-circuit a repeat
// ingest (client retry / SDK transport retry) back to the original document
// instead of creating a duplicate.
func (p *Pool) GetDocumentByIdempotencyKey(ctx context.Context, orgID, key string) (*Document, error) {
	if orgID == "" {
		return nil, fmt.Errorf("GetDocumentByIdempotencyKey: orgID is required")
	}
	if key == "" {
		return nil, ErrNotFound
	}
	row := p.QueryRow(ctx, `
        SELECT id, org_id, store_id, title, content_type, source_ref, status, error_message,
               byte_size, metadata, created_at, updated_at, toc_tree
        FROM documents WHERE org_id = $1 AND idempotency_key = $2`, orgID, key)

	var d Document
	var status string
	var rawMeta, rawTOC []byte
	if err := row.Scan(&d.ID, &d.OrgID, &d.StoreID, &d.Title, &d.ContentType, &d.SourceRef, &status,
		&d.ErrorMessage, &d.ByteSize, &rawMeta, &d.CreatedAt, &d.UpdatedAt, &rawTOC); err != nil {
		return nil, mapErr(err)
	}
	d.Status = DocumentStatus(status)
	d.Metadata = unmarshalMeta(rawMeta)
	d.TOCTree = rawTOC
	d.IdempotencyKey = key
	return &d, nil
}

// GetDocument fetches a document scoped to an org and (optionally) a
// store. storeID == "" means "don't filter by store" — used by
// header-less / pre-stores callers. A non-empty storeID restricts the
// lookup to that collection, so a document in store A is invisible
// when querying store B.
func (p *Pool) GetDocument(ctx context.Context, id tree.DocumentID, orgID, storeID string) (*Document, error) {
	if orgID == "" {
		return nil, fmt.Errorf("GetDocument: orgID is required")
	}
	q := `
        SELECT id, org_id, store_id, title, content_type, source_ref, status, error_message,
               byte_size, metadata, created_at, updated_at, toc_tree
        FROM documents WHERE id = $1 AND org_id = $2`
	args := []any{string(id), orgID}
	if storeID != "" {
		q += " AND store_id = $3"
		args = append(args, storeID)
	}
	row := p.QueryRow(ctx, q, args...)

	var d Document
	var status string
	var rawMeta, rawTOC []byte
	if err := row.Scan(&d.ID, &d.OrgID, &d.StoreID, &d.Title, &d.ContentType, &d.SourceRef, &status,
		&d.ErrorMessage, &d.ByteSize, &rawMeta, &d.CreatedAt, &d.UpdatedAt, &rawTOC); err != nil {
		return nil, mapErr(err)
	}
	d.Status = DocumentStatus(status)
	d.Metadata = unmarshalMeta(rawMeta)
	d.TOCTree = rawTOC
	return &d, nil
}

// GetDocumentForWorker is the un-scoped variant — used ONLY by the
// background ingest pipeline which is identified by its QStash
// signature rather than an X-Vectorless-Org header. Do NOT call this
// from any user-facing path.
func (p *Pool) GetDocumentForWorker(ctx context.Context, id tree.DocumentID) (*Document, error) {
	row := p.QueryRow(ctx, `
        SELECT id, org_id, store_id, title, content_type, source_ref, status, error_message,
               byte_size, metadata, created_at, updated_at, toc_tree
        FROM documents WHERE id = $1`, string(id))

	var d Document
	var status string
	var rawMeta, rawTOC []byte
	if err := row.Scan(&d.ID, &d.OrgID, &d.StoreID, &d.Title, &d.ContentType, &d.SourceRef, &status,
		&d.ErrorMessage, &d.ByteSize, &rawMeta, &d.CreatedAt, &d.UpdatedAt, &rawTOC); err != nil {
		return nil, mapErr(err)
	}
	d.Status = DocumentStatus(status)
	d.Metadata = unmarshalMeta(rawMeta)
	d.TOCTree = rawTOC
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

// UpdateDocumentTOCTree persists the LLM-built table-of-contents
// tree onto the documents.toc_tree column. treeJSON is the already
// JSON-marshalled []tree.TOCNode; pass a nil slice to clear (writes
// SQL NULL — the "not yet generated" state). Mirrors
// UpdateSectionSummaryAxes so the column can be patched
// independently of the rest of the document row.
func (p *Pool) UpdateDocumentTOCTree(ctx context.Context, id tree.DocumentID, treeJSON []byte) error {
	var arg any
	if len(treeJSON) > 0 {
		arg = treeJSON
	}
	_, err := p.Exec(ctx, `
        UPDATE documents
        SET toc_tree = $2, updated_at = now()
        WHERE id = $1`, string(id), arg)
	return mapErr(err)
}

// ListDocumentsOpts controls pagination + filtering for ListDocuments.
type ListDocumentsOpts struct {
	// OrgID restricts the listing to a single tenant. Required.
	OrgID string
	// StoreID restricts the listing to one collection. Empty means
	// "all stores in the org" (header-less / pre-stores callers).
	StoreID string
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
	if o.StoreID != "" {
		where += " AND store_id = $" + itoa(next)
		args = append(args, o.StoreID)
		next++
	}
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
        SELECT id, org_id, store_id, title, content_type, source_ref, status, error_message,
               byte_size, metadata, created_at, updated_at, toc_tree
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
		var rawMeta, rawTOC []byte
		if err := rows.Scan(&d.ID, &d.OrgID, &d.StoreID, &d.Title, &d.ContentType, &d.SourceRef, &status,
			&d.ErrorMessage, &d.ByteSize, &rawMeta, &d.CreatedAt, &d.UpdatedAt, &rawTOC); err != nil {
			return nil, time.Time{}, err
		}
		d.Status = DocumentStatus(status)
		d.Metadata = unmarshalMeta(rawMeta)
		d.TOCTree = rawTOC
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
// scoped to an org and (optionally) a store. Cross-scope deletes
// return ErrNotFound.
func (p *Pool) DeleteDocument(ctx context.Context, id tree.DocumentID, orgID, storeID string) error {
	if orgID == "" {
		return fmt.Errorf("DeleteDocument: orgID is required")
	}
	q := `DELETE FROM documents WHERE id = $1 AND org_id = $2`
	args := []any{string(id), orgID}
	if storeID != "" {
		q += " AND store_id = $3"
		args = append(args, storeID)
	}
	tag, err := p.Exec(ctx, q, args...)
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
