package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// QueryHandler implements the retrieval query endpoint.
type QueryHandler struct {
	logger   *slog.Logger
	db       *db.Pool
	storage  storage.Storage
	strategy retrieval.Strategy
}

// NewQueryHandler creates a QueryHandler.
func NewQueryHandler(
	logger *slog.Logger,
	pool *db.Pool,
	store storage.Storage,
	strategy retrieval.Strategy,
) *QueryHandler {
	return &QueryHandler{
		logger:   logger,
		db:       pool,
		storage:  store,
		strategy: strategy,
	}
}

// queryRequest is the JSON body for POST /v1/query.
type queryRequest struct {
	DocumentID        tree.DocumentID `json:"document_id"`
	Query             string          `json:"query"`
	Model             string          `json:"model"`
	MaxTokens         int             `json:"max_tokens"`
	ReservedForPrompt int             `json:"reserved_for_prompt"`
	MaxParallelCalls  int             `json:"max_parallel_calls"`
	MaxSections       int             `json:"max_sections"`
}

// HandleQuery accepts a query, loads the document tree, runs the
// configured retrieval strategy, and returns the selected sections
// with their full content.
func (h *QueryHandler) HandleQuery(w http.ResponseWriter, r *http.Request) {
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
	if h.strategy == nil {
		writeErr(w, http.StatusServiceUnavailable, "no retrieval strategy configured")
		return
	}

	t, err := h.db.LoadTree(r.Context(), body.DocumentID, orgID, storeID(r))
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

	started := time.Now()

	// Use CostStrategy if available to get token usage + cost.
	var (
		ids   []tree.SectionID
		usage *retrieval.Usage
	)
	if cs, ok := h.strategy.(retrieval.CostStrategy); ok {
		result, err := cs.SelectWithCost(r.Context(), t, body.Query, budget)
		if err != nil {
			h.logger.Error("query: strategy failed",
				"err", err,
				"document_id", body.DocumentID,
			)
			writeErr(w, http.StatusInternalServerError, "retrieval failed: "+err.Error())
			return
		}
		ids = result.SelectedIDs
		usage = &result.Usage
	} else {
		var err error
		ids, err = h.strategy.Select(r.Context(), t, body.Query, budget)
		if err != nil {
			h.logger.Error("query: strategy failed",
				"err", err,
				"document_id", body.DocumentID,
			)
			writeErr(w, http.StatusInternalServerError, "retrieval failed: "+err.Error())
			return
		}
	}

	if body.MaxSections > 0 && len(ids) > body.MaxSections {
		ids = ids[:body.MaxSections]
	}

	sections := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		sec := t.FindByID(id)
		if sec == nil {
			continue
		}
		var content string
		if sec.ContentRef != "" {
			rc, _, getErr := h.storage.Get(r.Context(), sec.ContentRef)
			if getErr == nil {
				raw, _ := io.ReadAll(rc)
				rc.Close()
				content = string(raw)
			}
		}
		sections = append(sections, map[string]any{
			"id":          sec.ID,
			"parent_id":   sec.ParentID,
			"title":       sec.Title,
			"summary":     sec.Summary,
			"token_count": sec.TokenCount,
			"content":     content,
		})
	}

	resp := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"strategy":    h.strategy.Name(),
		"model":       body.Model,
		"sections":    sections,
		"elapsed_ms":  time.Since(started).Milliseconds(),
	}
	if usage != nil {
		resp["usage"] = map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"total_tokens":  usage.TotalTokens,
			"cost_usd":      usage.CostUSD,
			"llm_calls":     usage.LLMCalls,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
