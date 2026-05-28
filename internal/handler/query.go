package handler

import (
	"context"
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
	// strategies is the pre-built set of selectable strategies keyed
	// by config name (chunked-tree, pageindex, agentic, single-pass).
	// A per-request "strategy" field selects one of these; an absent
	// or empty field falls back to the configured default (strategy).
	// Nil/empty disables the override entirely — every request uses
	// the default.
	strategies map[string]retrieval.Strategy

	// treeLoader is a test seam overriding how the handler resolves
	// the document tree. Nil routes through the org-scoped DB lookup
	// (the production path); tests set it to a deterministic in-memory
	// function so the handler runs end-to-end without a real Postgres
	// backend.
	treeLoader func(ctx context.Context, orgID, storeID string, docID tree.DocumentID) (*tree.Tree, error)
}

// loadTree resolves the document tree, routing through the test seam
// when set.
func (h *QueryHandler) loadTree(ctx context.Context, orgID, storeID string, docID tree.DocumentID) (*tree.Tree, error) {
	if h.treeLoader != nil {
		return h.treeLoader(ctx, orgID, storeID, docID)
	}
	return h.db.LoadTree(ctx, docID, orgID, storeID)
}

// NewQueryHandler creates a QueryHandler. strategies is the optional
// pre-built map that backs the per-request "strategy" override; pass
// nil to disable the override (every request uses the default
// strategy).
func NewQueryHandler(
	logger *slog.Logger,
	pool *db.Pool,
	store storage.Storage,
	strategy retrieval.Strategy,
	strategies map[string]retrieval.Strategy,
) *QueryHandler {
	return &QueryHandler{
		logger:     logger,
		db:         pool,
		storage:    store,
		strategy:   strategy,
		strategies: strategies,
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
	// Strategy optionally overrides the configured retrieval strategy
	// for THIS request only. One of: chunked-tree, pageindex, agentic,
	// single-pass. Empty uses the server default. This lets a caller
	// (e.g. the benchmark harness) A/B strategies against the same
	// running engine without a redeploy. Unknown values return 400.
	Strategy string `json:"strategy"`
}

// resolveStrategy picks the strategy for one request. An empty
// override yields the configured default. A non-empty override is
// looked up in the pre-built set; an unknown name (or an override on a
// handler with no strategy set wired) returns ok=false so the caller
// can reply 400 rather than silently falling back.
func (h *QueryHandler) resolveStrategy(override string) (retrieval.Strategy, bool) {
	if override == "" {
		return h.strategy, true
	}
	if h.strategies == nil {
		return nil, false
	}
	s, ok := h.strategies[override]
	return s, ok
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

	strategy, ok := h.resolveStrategy(body.Strategy)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown strategy: "+body.Strategy)
		return
	}
	if strategy == nil {
		writeErr(w, http.StatusServiceUnavailable, "no retrieval strategy configured")
		return
	}

	t, err := h.loadTree(r.Context(), orgID, storeID(r), body.DocumentID)
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
	if cs, ok := strategy.(retrieval.CostStrategy); ok {
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
		ids, err = strategy.Select(r.Context(), t, body.Query, budget)
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
		"strategy":    strategy.Name(),
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
