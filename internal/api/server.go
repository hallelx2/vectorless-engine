// Package api exposes the engine's v1 HTTP API.
//
// Routes are versioned under /v1 from day one. Breaking changes will ship
// under /v2 and run alongside /v1 through a deprecation window.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/hallelx2/vectorless-engine/internal/db"
	"github.com/hallelx2/vectorless-engine/internal/ingest"
	"github.com/hallelx2/vectorless-engine/internal/queue"
	"github.com/hallelx2/vectorless-engine/internal/retrieval"
	"github.com/hallelx2/vectorless-engine/internal/storage"
	"github.com/hallelx2/vectorless-engine/internal/tree"
)

// Deps bundles the engine's subsystems for injection into the API layer.
type Deps struct {
	Logger   *slog.Logger
	DB       *db.Pool
	Storage  storage.Storage
	Queue    queue.Queue
	Strategy retrieval.Strategy
	Version  string
}

// Router builds and returns the chi router wired with v1 routes.
func Router(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Heartbeat("/health"))

	r.Route("/v1", func(r chi.Router) {
		r.Get("/health", d.handleHealth)
		r.Get("/version", d.handleVersion)

		r.Route("/documents", func(r chi.Router) {
			r.Get("/", d.handleListDocuments)
			r.Post("/", d.handleIngestDocument)
			r.Get("/{id}", d.handleGetDocument)
			r.Delete("/{id}", d.handleDeleteDocument)
			r.Get("/{id}/tree", d.handleGetTree)
		})

		r.Get("/sections/{id}", d.handleGetSection)
		r.Post("/query", d.handleQuery)
	})

	r.Post("/internal/jobs/{kind}", d.handleQueueWebhook)

	return r
}

func (d Deps) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (d Deps) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": d.Version})
}

// --- ingest / documents ---

// handleListDocuments returns a page of documents, most-recent first.
// Query params: limit (1..200, default 50), status, cursor (RFC3339
// created_at from the previous page's next_cursor).
func (d Deps) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := db.ListDocumentsOpts{
		Status: db.DocumentStatus(q.Get("status")),
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

	docs, next, err := d.DB.ListDocuments(r.Context(), opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		items = append(items, map[string]any{
			"id":           doc.ID,
			"title":        doc.Title,
			"content_type": doc.ContentType,
			"status":       string(doc.Status),
			"byte_size":    doc.ByteSize,
			"created_at":   doc.CreatedAt,
			"updated_at":   doc.UpdatedAt,
		})
	}
	resp := map[string]any{"items": items}
	if !next.IsZero() {
		resp["next_cursor"] = next.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleIngestDocument accepts a document via either multipart/form-data
// (field name: "file") or a JSON body { "content": "...", "filename": "..." }.
// The bytes are streamed to Storage, a documents row is created in
// "pending" state, and an ingest job is enqueued.
func (d Deps) handleIngestDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	docID := ingest.NewDocumentID()

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
		defer file.Close()
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
	if err := d.Storage.Put(ctx, key, body, storage.Metadata{
		Key: key, Size: size, ContentType: contentType,
	}); err != nil {
		d.Logger.Error("ingest: storage put failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "storage write failed")
		return
	}

	title := filename
	if title == "" {
		title = string(docID)
	}

	if err := d.DB.NewDocument(ctx, db.Document{
		ID:          docID,
		Title:       title,
		ContentType: contentType,
		SourceRef:   key,
		Status:      db.StatusPending,
		ByteSize:    size,
	}); err != nil {
		d.Logger.Error("ingest: db insert failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "db write failed")
		return
	}

	payload, _ := json.Marshal(ingest.Payload{
		DocumentID:  docID,
		ContentType: contentType,
		Filename:    filename,
		SourceRef:   key,
	})
	if err := d.Queue.Enqueue(ctx, queue.Job{
		Kind:      queue.KindIngestDocument,
		Payload:   payload,
		DedupeKey: string(docID),
	}); err != nil {
		d.Logger.Error("ingest: enqueue failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "enqueue failed")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"document_id": docID,
		"status":      string(db.StatusPending),
	})
}

func (d Deps) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	id := tree.DocumentID(chi.URLParam(r, "id"))
	doc, err := d.DB.GetDocument(r.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            doc.ID,
		"title":         doc.Title,
		"content_type":  doc.ContentType,
		"status":        string(doc.Status),
		"byte_size":     doc.ByteSize,
		"error_message": doc.ErrorMessage,
		"metadata":      doc.Metadata,
		"created_at":    doc.CreatedAt,
		"updated_at":    doc.UpdatedAt,
	})
}

func (d Deps) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id := tree.DocumentID(chi.URLParam(r, "id"))
	if err := d.DB.DeleteDocument(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleGetTree(w http.ResponseWriter, r *http.Request) {
	id := tree.DocumentID(chi.URLParam(r, "id"))
	t, err := d.DB.LoadTree(r.Context(), id)
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

func (d Deps) handleGetSection(w http.ResponseWriter, r *http.Request) {
	id := tree.SectionID(chi.URLParam(r, "id"))
	sec, err := d.DB.GetSection(r.Context(), id)
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
		rc, _, err := d.Storage.Get(r.Context(), sec.ContentRef)
		if err == nil {
			raw, _ := io.ReadAll(rc)
			rc.Close()
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

// --- query ---

func (d Deps) handleQuery(w http.ResponseWriter, r *http.Request) {
	// TODO(phase-2): run strategy.Select against the loaded tree.
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "query: not implemented"})
}

// --- internal queue webhook ---

func (d Deps) handleQueueWebhook(w http.ResponseWriter, r *http.Request) {
	// TODO(phase-1): verify QStash signature, decode body, dispatch.
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func guessContentType(filename string) string {
	name := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(name, ".md"), strings.HasSuffix(name, ".markdown"):
		return "text/markdown"
	case strings.HasSuffix(name, ".txt"):
		return "text/plain"
	case strings.HasSuffix(name, ".html"), strings.HasSuffix(name, ".htm"):
		return "text/html"
	case strings.HasSuffix(name, ".pdf"):
		return "application/pdf"
	}
	return "application/octet-stream"
}
