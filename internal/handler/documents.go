package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/queue"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// DocumentsHandler implements the documents and sections API surface.
type DocumentsHandler struct {
	logger  *slog.Logger
	db      *db.Pool
	storage storage.Storage
	queue   queue.Queue
}

// NewDocumentsHandler creates a DocumentsHandler.
func NewDocumentsHandler(
	logger *slog.Logger,
	pool *db.Pool,
	store storage.Storage,
	q queue.Queue,
) *DocumentsHandler {
	return &DocumentsHandler{
		logger:  logger,
		db:      pool,
		storage: store,
		queue:   q,
	}
}

// requireOrgID pulls the X-Vectorless-Org header (injected by the
// control plane on every authenticated request) and writes a 400 if
// it's missing. Returns the org id and true on success.
func requireOrgID(w http.ResponseWriter, r *http.Request) (string, bool) {
	org := r.Header.Get("X-Vectorless-Org")
	if org == "" {
		writeErr(w, http.StatusBadRequest, "missing X-Vectorless-Org header")
		return "", false
	}
	return org, true
}

// storeID pulls the optional X-Vectorless-Store header. Unlike org,
// store is optional: an empty result means "don't scope by store"
// (header-less / pre-stores callers see the whole org). The control
// plane injects this once stores are wired.
func storeID(r *http.Request) string {
	return r.Header.Get("X-Vectorless-Store")
}

// HandleListDocuments returns a paginated list of documents for the
// org identified by the X-Vectorless-Org header.
//
// Query params:
//   - limit  — page size, clamped to [1, 200], default 50
//   - status — optional filter by lifecycle status
//   - cursor — RFC3339Nano timestamp from previous page's next_cursor
func (h *DocumentsHandler) HandleListDocuments(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	opts := db.ListDocumentsOpts{
		OrgID:   orgID,
		StoreID: storeID(r),
		Status:  db.DocumentStatus(q.Get("status")),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.Cursor = t
		}
	}

	docs, next, err := h.db.ListDocuments(r.Context(), opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		items = append(items, map[string]any{
			"doc_id":       doc.ID,
			"id":           doc.ID,
			"title":        doc.Title,
			"content_type": doc.ContentType,
			"source_type":  sourceTypeFromContentType(doc.ContentType),
			"status":       string(doc.Status),
			"byte_size":    doc.ByteSize,
			"created_at":   doc.CreatedAt,
			"updated_at":   doc.UpdatedAt,
		})
	}
	// Dashboard expects {documents, next_cursor, has_more}; the older
	// {items} shape is kept as an alias for any SDK callers that
	// already key off it.
	resp := map[string]any{
		"documents": items,
		"items":     items,
		"has_more":  !next.IsZero(),
	}
	if !next.IsZero() {
		resp["next_cursor"] = next.Format(time.RFC3339Nano)
	} else {
		resp["next_cursor"] = nil
	}
	writeJSON(w, http.StatusOK, resp)
}

// sourceTypeFromContentType collapses an HTTP Content-Type to the
// short tag the dashboard's source-type badge expects.
func sourceTypeFromContentType(ct string) string {
	switch {
	case strings.HasPrefix(ct, "application/pdf"):
		return "pdf"
	case strings.HasPrefix(ct, "text/markdown"):
		return "markdown"
	case strings.HasPrefix(ct, "text/plain"):
		return "text"
	case strings.HasPrefix(ct, "text/html"):
		return "html"
	case strings.HasPrefix(ct, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"):
		return "docx"
	default:
		return "file"
	}
}

// HandleIngestDocument accepts a document via either multipart/form-data
// (field name: "file") or JSON body {"content": "...", "filename": "..."}.
// Returns 202 Accepted with the document_id in "pending" state.
func (h *DocumentsHandler) HandleIngestDocument(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	docID := ingest.NewDocumentID()

	// Idempotent ingest: if the client supplied an Idempotency-Key and we
	// already have a document for this org under that key, return it instead
	// of creating a duplicate. This makes a client/transport retry of the
	// ingest POST (the HAL-323 duplicate-upload bug) a no-op.
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if existing, err := h.db.GetDocumentByIdempotencyKey(ctx, orgID, idemKey); err == nil {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"document_id": existing.ID,
				"status":      string(existing.Status),
			})
			return
		} else if !errors.Is(err, db.ErrNotFound) {
			h.logger.Error("ingest: idempotency lookup failed", "err", err)
			writeErr(w, http.StatusInternalServerError, "db read failed")
			return
		}
	}

	var (
		filename    string
		contentType string
		body        io.Reader
		size        int64
	)

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid multipart body: "+err.Error())
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, `missing form field "file"`)
			return
		}
		defer func() { _ = file.Close() }() // best-effort close
		filename = header.Filename
		contentType = header.Header.Get("Content-Type")
		body = file
		size = header.Size

	case strings.HasPrefix(ct, "application/json"):
		var payload struct {
			Filename    string `json:"filename"`
			ContentType string `json:"content_type"`
			Content     string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		if payload.Content == "" {
			writeErr(w, http.StatusBadRequest, `"content" is required`)
			return
		}
		filename = payload.Filename
		contentType = payload.ContentType
		body = strings.NewReader(payload.Content)
		size = int64(len(payload.Content))

	default:
		writeErr(w, http.StatusUnsupportedMediaType,
			"use multipart/form-data (file) or application/json (content)")
		return
	}

	if contentType == "" {
		contentType = guessContentType(filename)
	}

	key := ingest.SourceKey(docID, filename)
	if err := h.storage.Put(ctx, key, body, storage.Metadata{
		Key: key, Size: size, ContentType: contentType,
	}); err != nil {
		h.logger.Error("ingest: storage put failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "storage write failed")
		return
	}

	title := filename
	if title == "" {
		title = string(docID)
	}

	if err := h.db.NewDocument(ctx, db.Document{
		ID:             docID,
		OrgID:          orgID,
		StoreID:        storeID(r),
		Title:          title,
		ContentType:    contentType,
		SourceRef:      key,
		Status:         db.StatusPending,
		ByteSize:       size,
		IdempotencyKey: idemKey,
	}); err != nil {
		// A concurrent request with the same Idempotency-Key won the race and
		// inserted first. Drop the orphan source we just wrote and return the
		// winner's document rather than a 500.
		if idemKey != "" && errors.Is(err, db.ErrConflict) {
			if delErr := h.storage.Delete(ctx, key); delErr != nil {
				h.logger.Warn("ingest: orphan source cleanup failed", "err", delErr, "source_ref", key, "idempotency_key", idemKey)
			}
			if existing, lookupErr := h.db.GetDocumentByIdempotencyKey(ctx, orgID, idemKey); lookupErr == nil {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"document_id": existing.ID,
					"status":      string(existing.Status),
				})
				return
			}
		}
		h.logger.Error("ingest: db insert failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "db write failed")
		return
	}

	payload, _ := json.Marshal(ingest.Payload{
		DocumentID:  docID,
		ContentType: contentType,
		Filename:    filename,
		SourceRef:   key,
		Profile:     r.Header.Get("X-Vectorless-Profile"),
	})
	if err := h.queue.Enqueue(ctx, queue.Job{
		Kind:      queue.KindIngestDocument,
		Payload:   payload,
		DedupeKey: string(docID),
	}); err != nil {
		h.logger.Error("ingest: enqueue failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "enqueue failed")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"document_id": docID,
		"status":      string(db.StatusPending),
	})
}

// HandleGetDocument returns metadata and lifecycle status for one document.
func (h *DocumentsHandler) HandleGetDocument(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	id := tree.DocumentID(chi.URLParam(r, "id"))
	doc, err := h.db.GetDocument(r.Context(), id, orgID, storeID(r))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Section count is fire-and-forget — if it fails we still return
	// the rest of the doc rather than failing the whole GET.
	sectionCount := 0
	if n, cerr := h.db.CountSections(r.Context(), id, orgID, storeID(r)); cerr == nil {
		sectionCount = n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"doc_id":        doc.ID,
		"id":            doc.ID,
		"title":         doc.Title,
		"content_type":  doc.ContentType,
		"source_type":   sourceTypeFromContentType(doc.ContentType),
		"status":        string(doc.Status),
		"byte_size":     doc.ByteSize,
		"section_count": sectionCount,
		"error_message": doc.ErrorMessage,
		"metadata":      doc.Metadata,
		"created_at":    doc.CreatedAt,
		"updated_at":    doc.UpdatedAt,
	})
}

// HandleDeleteDocument removes a document and cascades to its sections.
func (h *DocumentsHandler) HandleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	id := tree.DocumentID(chi.URLParam(r, "id"))
	if err := h.db.DeleteDocument(r.Context(), id, orgID, storeID(r)); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleGetDocumentSource streams the original document bytes back to the
// caller. The document's SourceRef points to its location in storage.
func (h *DocumentsHandler) HandleGetDocumentSource(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	id := tree.DocumentID(chi.URLParam(r, "id"))
	doc, err := h.db.GetDocument(r.Context(), id, orgID, storeID(r))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if doc.SourceRef == "" {
		writeErr(w, http.StatusNotFound, "document source not available")
		return
	}

	rc, meta, err := h.storage.Get(r.Context(), doc.SourceRef)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read source: "+err.Error())
		return
	}
	defer func() { _ = rc.Close() }() // best-effort close

	ct := doc.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if meta.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	}
	if doc.Title != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, doc.Title))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc) // best-effort write to response
}

// HandleGetTree returns the compact tree view used for LLM reasoning.
func (h *DocumentsHandler) HandleGetTree(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	id := tree.DocumentID(chi.URLParam(r, "id"))
	t, err := h.db.LoadTree(r.Context(), id, orgID, storeID(r))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t.BuildView())
}

// HandleGetLlmsTxt renders the document tree as an llms.txt Markdown map —
// the navigable, LLM-friendly index of the document (H1 title, blockquote
// summary, nested section headings with one-line summaries).
func (h *DocumentsHandler) HandleGetLlmsTxt(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	id := tree.DocumentID(chi.URLParam(r, "id"))
	t, err := h.db.LoadTree(r.Context(), id, orgID, storeID(r))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, t.RenderLLMSTxt())
}

// HandleGetSection returns a single section with full content from storage.
func (h *DocumentsHandler) HandleGetSection(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	id := tree.SectionID(chi.URLParam(r, "id"))
	sec, err := h.db.GetSection(r.Context(), id, orgID, storeID(r))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "section not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var content string
	if sec.ContentRef != "" {
		rc, _, getErr := h.storage.Get(r.Context(), sec.ContentRef)
		if getErr == nil {
			raw, _ := io.ReadAll(rc)
			_ = rc.Close() // best-effort close
			content = string(raw)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          sec.ID,
		"document_id": sec.DocumentID,
		"parent_id":   sec.ParentID,
		"ordinal":     sec.Ordinal,
		"depth":       sec.Depth,
		"title":       sec.Title,
		"summary":     sec.Summary,
		"token_count": sec.TokenCount,
		"metadata":    sec.Metadata,
		"content":     content,
	})
}
