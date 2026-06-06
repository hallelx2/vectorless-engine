package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// QueryMultiHandler implements the multi-document retrieval query endpoint.
type QueryMultiHandler struct {
	logger   *slog.Logger
	storage  storage.Storage
	strategy retrieval.Strategy
	multiDoc *retrieval.MultiDoc
}

// NewQueryMultiHandler creates a QueryMultiHandler.
func NewQueryMultiHandler(
	logger *slog.Logger,
	store storage.Storage,
	strategy retrieval.Strategy,
	multiDoc *retrieval.MultiDoc,
) *QueryMultiHandler {
	return &QueryMultiHandler{
		logger:   logger,
		storage:  store,
		strategy: strategy,
		multiDoc: multiDoc,
	}
}

// queryMultiRequest is the JSON body for POST /v1/query/multi.
type queryMultiRequest struct {
	DocumentIDs       []tree.DocumentID `json:"document_ids"`
	Query             string            `json:"query"`
	Model             string            `json:"model"`
	MaxTokens         int               `json:"max_tokens"`
	ReservedForPrompt int               `json:"reserved_for_prompt"`
	MaxParallelCalls  int               `json:"max_parallel_calls"`
	MaxSections       int               `json:"max_sections"`
}

// HandleQueryMulti accepts a query with multiple document IDs, fans out
// the retrieval strategy across all documents in parallel, and returns
// per-document results.
func (h *QueryMultiHandler) HandleQueryMulti(w http.ResponseWriter, r *http.Request) {
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

	started := time.Now()
	result, err := h.multiDoc.Query(r.Context(), orgID, storeID(r), body.DocumentIDs, body.Query, budget)
	if err != nil {
		h.logger.Error("query/multi: failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "multi-doc retrieval failed: "+err.Error())
		return
	}

	// Build per-document response.
	docs := make([]map[string]any, 0, len(result.Documents))
	for docID, dr := range result.Documents {
		sections := make([]map[string]any, 0, len(dr.SelectedIDs))
		for _, sid := range dr.SelectedIDs {
			sec := dr.Tree.FindByID(sid)
			if sec == nil {
				continue
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
			sections = append(sections, map[string]any{
				"id":          sec.ID,
				"parent_id":   sec.ParentID,
				"title":       sec.Title,
				"summary":     sec.Summary,
				"token_count": sec.TokenCount,
				"content":     content,
			})
			if body.MaxSections > 0 && len(sections) >= body.MaxSections {
				break
			}
		}
		docs = append(docs, map[string]any{
			"document_id": docID,
			"sections":    sections,
			"usage": map[string]any{
				"input_tokens":  dr.Usage.InputTokens,
				"output_tokens": dr.Usage.OutputTokens,
				"total_tokens":  dr.Usage.TotalTokens,
				"cost_usd":      dr.Usage.CostUSD,
				"llm_calls":     dr.Usage.LLMCalls,
			},
		})
	}

	// Per-document errors.
	errs := make(map[string]string, len(result.Errors))
	for docID, e := range result.Errors {
		errs[string(docID)] = e.Error()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"query":      body.Query,
		"strategy":   h.strategy.Name(),
		"model":      body.Model,
		"documents":  docs,
		"errors":     errs,
		"elapsed_ms": time.Since(started).Milliseconds(),
		"total_usage": map[string]any{
			"input_tokens":  result.TotalUsage.InputTokens,
			"output_tokens": result.TotalUsage.OutputTokens,
			"total_tokens":  result.TotalUsage.TotalTokens,
			"cost_usd":      result.TotalUsage.CostUSD,
			"llm_calls":     result.TotalUsage.LLMCalls,
		},
	})
}
