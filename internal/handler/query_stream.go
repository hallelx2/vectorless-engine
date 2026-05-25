package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
)

// QueryStreamHandler implements the SSE streaming query endpoint for
// non-Connect clients (curl, browsers, SDKs that prefer SSE over gRPC).
type QueryStreamHandler struct {
	logger   *slog.Logger
	db       *db.Pool
	storage  storage.Storage
	strategy retrieval.Strategy
}

// NewQueryStreamHandler creates a QueryStreamHandler.
func NewQueryStreamHandler(
	logger *slog.Logger,
	pool *db.Pool,
	store storage.Storage,
	strategy retrieval.Strategy,
) *QueryStreamHandler {
	return &QueryStreamHandler{
		logger:   logger,
		db:       pool,
		storage:  store,
		strategy: strategy,
	}
}

// sseEvent is a single Server-Sent Event in the query stream.
type sseEvent struct {
	Type        string `json:"type"`
	Section     any    `json:"section,omitempty"`
	SliceIndex  int    `json:"slice_index,omitempty"`
	TotalSlices int    `json:"total_slices,omitempty"`
	Message     string `json:"message,omitempty"`
	Strategy    string `json:"strategy,omitempty"`
	DocumentID  string `json:"document_id,omitempty"`
	Query       string `json:"query,omitempty"`
	ElapsedMS   int64  `json:"elapsed_ms,omitempty"`
}

// HandleQueryStream serves POST /v1/query/stream as Server-Sent Events.
//
// The response is text/event-stream. Each event is a JSON object
// prefixed with "data: " and terminated with "\n\n" per the SSE spec.
func (h *QueryStreamHandler) HandleQueryStream(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	var body queryRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.DocumentID == "" || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_id and query are required")
		return
	}

	// Check streaming support.
	ss, ok := h.strategy.(retrieval.StreamStrategy)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "strategy does not support streaming")
		return
	}

	ctx := r.Context()

	t, err := h.db.LoadTree(ctx, body.DocumentID, orgID, storeID(r))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	budget := retrieval.ContextBudget{
		ModelName:         body.Model,
		MaxTokens:         body.MaxTokens,
		ReservedForPrompt: body.ReservedForPrompt,
		MaxParallelCalls:  body.MaxParallelCalls,
	}
	if budget.MaxTokens == 0 {
		budget.MaxTokens = 100000
	}
	if budget.ReservedForPrompt == 0 {
		budget.ReservedForPrompt = 4000
	}
	if budget.MaxParallelCalls == 0 {
		budget.MaxParallelCalls = 8
	}

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	started := time.Now()
	events := ss.SelectStream(ctx, t, body.Query, budget)

	for evt := range events {
		sse := sseEvent{
			Type:        string(evt.Type),
			SliceIndex:  evt.SliceIndex,
			TotalSlices: evt.TotalSlices,
			Message:     evt.Message,
		}

		switch evt.Type {
		case retrieval.EventStarted:
			sse.Strategy = ss.Name()
			sse.DocumentID = string(body.DocumentID)
			sse.Query = body.Query

		case retrieval.EventSectionSelected:
			sec := t.FindByID(evt.SectionID)
			if sec != nil {
				var content string
				if sec.ContentRef != "" {
					rc, _, getErr := h.storage.Get(ctx, sec.ContentRef)
					if getErr == nil {
						raw, _ := io.ReadAll(rc)
						rc.Close()
						content = string(raw)
					}
				}
				sse.Section = map[string]any{
					"id":          sec.ID,
					"parent_id":   sec.ParentID,
					"title":       sec.Title,
					"summary":     sec.Summary,
					"token_count": sec.TokenCount,
					"content":     content,
				}
			}

		case retrieval.EventCompleted:
			sse.ElapsedMS = time.Since(started).Milliseconds()

		case retrieval.EventError:
			if evt.Error != nil {
				sse.Message = evt.Error.Error()
			}
		}

		data, _ := json.Marshal(sse)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, data)
		if canFlush {
			flusher.Flush()
		}
	}
}
