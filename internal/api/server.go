// Package api exposes the engine's v1 HTTP API.
//
// Routes are versioned under /v1 from day one. Breaking changes will ship
// under /v2 and run alongside /v1 through a deprecation window.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/hallelx2/vectorless-engine/internal/queue"
	"github.com/hallelx2/vectorless-engine/internal/retrieval"
	"github.com/hallelx2/vectorless-engine/internal/storage"
)

// Deps bundles the engine's subsystems for injection into the API layer.
type Deps struct {
	Logger   *slog.Logger
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
	r.Use(middleware.Heartbeat("/health")) // unversioned root health

	r.Route("/v1", func(r chi.Router) {
		r.Get("/health", d.handleHealth)
		r.Get("/version", d.handleVersion)

		r.Route("/documents", func(r chi.Router) {
			r.Post("/", d.handleIngestDocument)
			r.Get("/{id}", d.handleGetDocument)
			r.Delete("/{id}", d.handleDeleteDocument)
			r.Get("/{id}/tree", d.handleGetTree)
		})

		r.Get("/sections/{id}", d.handleGetSection)
		r.Post("/query", d.handleQuery)
	})

	// Internal webhook for QStash to deliver jobs to the engine when the
	// queue driver is qstash. Authenticated via QStash signatures in Phase 1.
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

func (d Deps) handleIngestDocument(w http.ResponseWriter, r *http.Request) {
	// TODO(phase-1): accept multipart upload or JSON { url, content_type },
	// persist raw bytes via d.Storage, insert document row, enqueue
	// KindIngestDocument job on d.Queue, return 202 with document_id.
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func (d Deps) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func (d Deps) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func (d Deps) handleGetTree(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func (d Deps) handleGetSection(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

// --- query ---

func (d Deps) handleQuery(w http.ResponseWriter, r *http.Request) {
	// TODO(phase-1):
	//   1. Parse { document_id, query, model?, strategy? } from r.Body.
	//   2. Load the tree for document_id from the database.
	//   3. Call d.Strategy.Select(ctx, tree, query, budget).
	//   4. Fetch the selected sections' content from d.Storage.
	//   5. Return { sections: [...] }.
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

// --- internal queue webhook ---

func (d Deps) handleQueueWebhook(w http.ResponseWriter, r *http.Request) {
	// TODO(phase-1): verify QStash signature, decode body, dispatch to
	// registered handler via d.Queue.(*queue.QStash).Dispatch.
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
