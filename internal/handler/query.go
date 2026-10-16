package handler

import (
	"context"
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
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// QueryHandler implements the retrieval query endpoint.
type QueryHandler struct {
	logger   *slog.Logger
	db       *db.Pool
	storage  storage.Storage
	strategy retrieval.Strategy
	// strategies is the pre-built set of selectable strategies keyed
	// by config name (chunked-tree, treewalk, agentic, single-pass).
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
	// for THIS request only. One of: auto, single-pass, chunked-tree,
	// agentic, treewalk. Empty uses the server default. This lets a caller
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
	s, ok := h.strategies[retrieval.InternalName(override)]
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
		ids      []tree.SectionID
		usage    *retrieval.Usage
		evidence []retrieval.EvidencePage
		full     *retrieval.Result
	)
	if cs, ok := strategy.(retrieval.CostStrategy); ok {
		result, err := cs.SelectWithCost(r.Context(), t, body.Query, budget)
		if err != nil {
			h.logger.Error("query: strategy failed",
				"err", err,
				"document_id", body.DocumentID,
			)
			writeErr(w, http.StatusInternalServerError, retrieval.PublicError(err))
			return
		}
		ids = result.SelectedIDs
		usage = &result.Usage
		evidence = result.EvidencePages
		full = result
	} else {
		var err error
		ids, err = strategy.Select(r.Context(), t, body.Query, budget)
		if err != nil {
			h.logger.Error("query: strategy failed",
				"err", err,
				"document_id", body.DocumentID,
			)
			writeErr(w, http.StatusInternalServerError, retrieval.PublicError(err))
			return
		}
	}

	// A page-based strategy's evidence pages lead the list, as found: the
	// page the Judge chose holds the answer, while the sections overlapping
	// it come back in document order (HAL-1390). max_sections caps the
	// combined list, so a small cap keeps the evidence, not the first
	// sections of the document (HAL-3071). Every unit carries body text: a
	// heading-only section, or one the pages already carry, gives its slot
	// to the next-ranked evidence (HAL-3160).
	res := &retrieval.Result{SelectedIDs: ids, EvidencePages: evidence}
	if full != nil {
		res = full
	}
	units, unitStats := retrieval.AssembleUnits(r.Context(), t, res, body.MaxSections, h.loadContent)
	sections := unitMaps(units)
	resp := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"strategy":    retrieval.PublicName(strategy.Name()),
		"model":       body.Model,
		"sections":    sections,
		"elapsed_ms":  time.Since(started).Milliseconds(),
		"unit_stats":  unitStats,
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
	withTiming(r, resp)
	setTokenHeaders(w.Header(), resp)
	writeJSON(w, http.StatusOK, resp)
}

// loadContent reads a section's stored text.
func (h *QueryHandler) loadContent(ctx context.Context, ref string) (string, error) {
	return readStored(ctx, h.storage, ref)
}

// readStored reads one stored object as text.
func readStored(ctx context.Context, store storage.Storage, ref string) (string, error) {
	rc, _, err := store.Get(ctx, ref)
	if err != nil {
		return "", err
	}
	defer rc.Close() // best-effort close
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// unitMaps renders assembled units in the response shape: a page as
// evidencePageSections renders it, a section with its tree fields. A unit
// that fills a slot below the evidence says so (runner_up), and a heading
// whose text was gathered from the sections beneath it says that
// (expanded).
func unitMaps(units []retrieval.Unit) []map[string]any {
	out := make([]map[string]any, 0, len(units))
	for _, u := range units {
		var m map[string]any
		if u.Kind == retrieval.UnitPage {
			m = evidencePageSections([]retrieval.EvidencePage{u.Page})[0]
		} else {
			m = map[string]any{
				"id":          u.Section.ID,
				"parent_id":   u.Section.ParentID,
				"title":       u.Section.Title,
				"summary":     u.Section.Summary,
				"token_count": u.Section.TokenCount,
				"content":     u.Content,
			}
			if u.Section.PageStart > 0 {
				m["page_start"] = u.Section.PageStart
			}
			if u.Section.PageEnd > 0 {
				m["page_end"] = u.Section.PageEnd
			}
			if u.Expanded {
				m["expanded"] = true
			}
		}
		if u.RunnerUp {
			m["runner_up"] = true
		}
		out = append(out, m)
	}
	return out
}

// evidencePageSections turns a page-based strategy's evidence pages into
// response units: id "page_<n>", the owning section's title with the page
// ("<section> · p.<n>"), the page text as content. The same shape the
// standalone engine returns (internal/api), so clients and the benchmark
// read one contract from either binary.
func evidencePageSections(pages []retrieval.EvidencePage) []map[string]any {
	out := make([]map[string]any, 0, len(pages))
	for _, ep := range pages {
		title := ep.Title
		if title == "" {
			title = fmt.Sprintf("Page %d", ep.Page)
		}
		out = append(out, map[string]any{
			"id":          fmt.Sprintf("page_%d", ep.Page),
			"title":       fmt.Sprintf("%s · p.%d", title, ep.Page),
			"content":     ep.Text,
			"page":        ep.Page,
			"confidence":  ep.Confidence,
			"token_count": len(ep.Text) / 4,
		})
	}
	return out
}
