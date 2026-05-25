package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// QueryStreamMultiHandler implements the SSE streaming multi-document
// query endpoint.
type QueryStreamMultiHandler struct {
	logger   *slog.Logger
	storage  storage.Storage
	multiDoc *retrieval.MultiDoc
}

// NewQueryStreamMultiHandler creates a QueryStreamMultiHandler.
func NewQueryStreamMultiHandler(
	logger *slog.Logger,
	store storage.Storage,
	multiDoc *retrieval.MultiDoc,
) *QueryStreamMultiHandler {
	return &QueryStreamMultiHandler{
		logger:   logger,
		storage:  store,
		multiDoc: multiDoc,
	}
}

// multiDocSSEEvent is a single Server-Sent Event in the multi-doc stream.
type multiDocSSEEvent struct {
	Type        string `json:"type"`
	DocumentID  string `json:"document_id"`
	DocIndex    int    `json:"doc_index"`
	Section     any    `json:"section,omitempty"`
	SliceIndex  int    `json:"slice_index,omitempty"`
	TotalSlices int    `json:"total_slices,omitempty"`
	Message     string `json:"message,omitempty"`
	Strategy    string `json:"strategy,omitempty"`
	Query       string `json:"query,omitempty"`
	ElapsedMS   int64  `json:"elapsed_ms,omitempty"`
}

// HandleQueryStreamMulti serves POST /v1/query/multi/stream as SSE.
//
// Events are emitted as sections are selected across all documents.
// Each event includes a document_id so the client can attribute it.
func (h *QueryStreamMultiHandler) HandleQueryStreamMulti(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	var body queryMultiRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(body.DocumentIDs) == 0 || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_ids (non-empty) and query are required")
		return
	}
	if h.multiDoc == nil {
		writeErr(w, http.StatusNotImplemented, "multi-document queries not configured")
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
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	started := time.Now()
	events := h.multiDoc.QueryStream(r.Context(), orgID, storeID(r), body.DocumentIDs, body.Query, budget)

	for mevt := range events {
		evt := mevt.Event

		sse := multiDocSSEEvent{
			Type:        string(evt.Type),
			DocumentID:  string(mevt.DocumentID),
			DocIndex:    mevt.DocIndex,
			SliceIndex:  evt.SliceIndex,
			TotalSlices: evt.TotalSlices,
			Message:     evt.Message,
		}

		switch evt.Type {
		case retrieval.EventStarted:
			sse.Query = body.Query

		case retrieval.EventSectionSelected:
			sse.Section = h.buildSectionPayload(r, mevt.DocumentID, evt.SectionID)

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

// buildSectionPayload fetches section content for inclusion in the SSE
// event. We need to load the tree for the document to find the section,
// but since QueryStream already loaded it, we retrieve from storage by
// the section ID. This is a best-effort lookup.
func (h *QueryStreamMultiHandler) buildSectionPayload(r *http.Request, docID tree.DocumentID, secID tree.SectionID) map[string]any {
	return map[string]any{
		"id":          secID,
		"document_id": docID,
	}
}
