package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hallelx2/llmgate"

	enginecfg "github.com/hallelx2/vectorless-engine/pkg/config"
	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// AnswerPageIndexHandler implements POST /v1/answer/pageindex: it runs
// the PageIndex agentic loop end-to-end and returns the model's answer
// plus page-grounded citations in one round-trip. Unlike /v1/answer,
// the loop owns the answer — there is no separate synthesis call.
//
// Ported from cmd/engine's internal/api.handleAnswerPageIndex, adapted
// to the deployed server's multi-tenant model (org + store from
// headers).
type AnswerPageIndexHandler struct {
	logger     *slog.Logger
	db         *db.Pool
	storage    storage.Storage
	llm        llmgate.Client
	llmModel   string
	answerSpan enginecfg.AnswerSpanBlock
	replay     retrieval.ReplayStore
	strategy   *retrieval.PageIndexStrategy
	pageIndex  enginecfg.PageIndexBlock

	// treeLoader is a test seam overriding how the handler resolves
	// the document tree. Nil routes through the org-scoped DB lookup
	// (the production path). Tests set it to a deterministic in-memory
	// function so the handler can run end-to-end via httptest without
	// a real Postgres backend.
	treeLoader func(ctx context.Context, orgID, storeID string, docID tree.DocumentID) (*tree.Tree, error)
}

// NewAnswerPageIndexHandler creates an AnswerPageIndexHandler. llm,
// replay, and strategy may be nil; a nil llm or strategy (or
// PageIndex.Enabled=false) makes the endpoint return 501.
func NewAnswerPageIndexHandler(
	logger *slog.Logger,
	pool *db.Pool,
	store storage.Storage,
	llm llmgate.Client,
	llmModel string,
	answerSpan enginecfg.AnswerSpanBlock,
	replay retrieval.ReplayStore,
	strategy *retrieval.PageIndexStrategy,
	pageIndex enginecfg.PageIndexBlock,
) *AnswerPageIndexHandler {
	return &AnswerPageIndexHandler{
		logger:     logger,
		db:         pool,
		storage:    store,
		llm:        llm,
		llmModel:   llmModel,
		answerSpan: answerSpan,
		replay:     replay,
		strategy:   strategy,
		pageIndex:  pageIndex,
	}
}

// loadTree resolves the document tree for the pageindex answer
// endpoint, routing through the test seam when set.
func (h *AnswerPageIndexHandler) loadTree(ctx context.Context, orgID, storeID string, docID tree.DocumentID) (*tree.Tree, error) {
	if h.treeLoader != nil {
		return h.treeLoader(ctx, orgID, storeID, docID)
	}
	return h.db.LoadTree(ctx, docID, orgID, storeID)
}

// pageIndexAnswerRequest is the body shape for /v1/answer/pageindex.
type pageIndexAnswerRequest struct {
	DocumentID       tree.DocumentID `json:"document_id"`
	Query            string          `json:"query"`
	Model            string          `json:"model"`
	MaxHops          int             `json:"max_hops"`
	MaxPagesPerFetch int             `json:"max_pages_per_fetch"` // chars cap; named per the spec
	Stream           bool            `json:"stream"`
	IncludeReasoning bool            `json:"reasoning"`
}

// HandleAnswerPageIndex runs the PageIndex agentic loop and returns
// the answer + page-grounded citations. Supports an SSE streaming
// variant (stream=true) and an opt-in reasoning trace
// (reasoning=true, or ?reasoning=true).
func (h *AnswerPageIndexHandler) HandleAnswerPageIndex(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	if h.llm == nil {
		writeErr(w, http.StatusNotImplemented, "answer/pageindex endpoint requires an LLM client")
		return
	}
	if h.strategy == nil || !h.pageIndex.Enabled {
		writeErr(w, http.StatusNotImplemented, "pageindex strategy not configured on this server (retrieval.pageindex.enabled=false)")
		return
	}

	var body pageIndexAnswerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.DocumentID == "" || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_id and query are required")
		return
	}
	// Allow ?reasoning=true as an alternative to the body field.
	if r.URL.Query().Get("reasoning") == "true" {
		body.IncludeReasoning = true
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

	// Build a per-request copy of the shared strategy so per-request
	// overrides (max_hops, max_pages_per_fetch, OnEvent, and the
	// org-scoped TOC provider) never mutate the shared instance that
	// other goroutines read concurrently.
	perReq := *h.strategy
	if body.MaxHops > 0 {
		perReq.MaxHops = body.MaxHops
	}
	if body.MaxPagesPerFetch > 0 {
		perReq.PageContentLimit = body.MaxPagesPerFetch
	}
	// Scope the TOC provider to the requesting org/store so the
	// get_document_structure tool reads only this tenant's
	// documents.toc_tree. Without a DB handle (tests) the strategy
	// keeps whatever TOC it was constructed with (often nil →
	// synthesised view).
	if h.db != nil {
		perReq.TOC = scopedTOCProvider{db: h.db, orgID: orgID, storeID: storeID(r)}
	}

	budget := retrieval.ContextBudget{ModelName: body.Model}
	if budget.ModelName == "" {
		budget.ModelName = h.llmModel
	}

	started := time.Now()

	// Stream variant: hijack the response writer for SSE.
	if body.Stream {
		h.serveStream(w, r, &perReq, t, body, budget, started)
		return
	}

	// Non-streaming: optionally capture the reasoning trace.
	var (
		traceMu sync.Mutex
		trace   []map[string]any
	)
	if body.IncludeReasoning {
		perReq.OnEvent = func(ev retrieval.PageIndexEvent) {
			traceMu.Lock()
			defer traceMu.Unlock()
			trace = append(trace, pageIndexEventToTraceMap(ev))
		}
	}

	res, err := perReq.SelectWithCost(r.Context(), t, body.Query, budget)
	if err != nil {
		h.logger.Error("answer/pageindex: strategy failed", "err", err, "document_id", body.DocumentID)
		writeErr(w, http.StatusInternalServerError, "pageindex strategy failed: "+err.Error())
		return
	}

	citations := h.buildCitations(r.Context(), t, res, body.Query, body.Model)

	resp := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"answer":      res.Reasoning, // strategy stores the agent's answer here
		"citations":   citations,
		"strategy":    perReq.Name(),
		"model":       budget.ModelName,
		"hops_taken":  res.HopsTaken,
		"usage": map[string]any{
			"input_tokens":  res.Usage.InputTokens,
			"output_tokens": res.Usage.OutputTokens,
			"total_tokens":  res.Usage.TotalTokens,
			"cost_usd":      res.Usage.CostUSD,
			"llm_calls":     res.Usage.LLMCalls,
		},
		"elapsed_ms":  time.Since(started).Milliseconds(),
		"trace_token": res.TraceToken,
		"pages_read":  res.PagesRead,
	}
	if body.IncludeReasoning && len(trace) > 0 {
		resp["reasoning_trace"] = trace
	}

	finalIDs := append([]tree.SectionID(nil), res.SelectedIDs...)
	raw, err := marshalJSONForReplay(resp)
	if err != nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeJSONWithReplay(w, h.replay, http.StatusOK, raw, res.TraceToken, retrieval.ReplayEntry{
		DocumentID:  body.DocumentID,
		Query:       body.Query,
		Model:       budget.ModelName,
		SelectedIDs: finalIDs,
	})
}

// serveStream handles the stream=true SSE variant. Each tool call
// emits one event so the caller can watch navigation in real time;
// the final "answer" event carries the full JSON response.
func (h *AnswerPageIndexHandler) serveStream(w http.ResponseWriter, r *http.Request, strat *retrieval.PageIndexStrategy, t *tree.Tree, body pageIndexAnswerRequest, budget retrieval.ContextBudget, started time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming requires http.Flusher; response writer does not support it")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var writeMu sync.Mutex
	emitSSE := func(eventType string, payload any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, raw)
		flusher.Flush()
	}

	strat.OnEvent = func(ev retrieval.PageIndexEvent) {
		emitSSE(ev.Type, ev)
	}

	emitSSE("started", map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"strategy":    strat.Name(),
		"model":       budget.ModelName,
	})

	res, err := strat.SelectWithCost(r.Context(), t, body.Query, budget)
	if err != nil {
		emitSSE("error", map[string]string{"error": err.Error()})
		return
	}

	citations := h.buildCitations(r.Context(), t, res, body.Query, body.Model)
	final := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"answer":      res.Reasoning,
		"citations":   citations,
		"strategy":    strat.Name(),
		"model":       budget.ModelName,
		"hops_taken":  res.HopsTaken,
		"usage": map[string]any{
			"input_tokens":  res.Usage.InputTokens,
			"output_tokens": res.Usage.OutputTokens,
			"total_tokens":  res.Usage.TotalTokens,
			"cost_usd":      res.Usage.CostUSD,
			"llm_calls":     res.Usage.LLMCalls,
		},
		"elapsed_ms":  time.Since(started).Milliseconds(),
		"trace_token": res.TraceToken,
		"pages_read":  res.PagesRead,
	}
	emitSSE("answer", final)
}

// buildCitations transforms the strategy's PagesRead + the section
// tree into the response's citations array: one citation per unique
// cited page range, each carrying the overlapping section IDs and a
// best-effort grounding quote extracted from the cited content.
func (h *AnswerPageIndexHandler) buildCitations(ctx context.Context, t *tree.Tree, res *retrieval.Result, query, requestModel string) []map[string]any {
	if res == nil {
		return nil
	}
	seen := make(map[[2]int]struct{}, len(res.PagesRead))
	citations := make([]map[string]any, 0, len(res.PagesRead))

	for _, pr := range res.PagesRead {
		key := [2]int{pr.StartPage, pr.EndPage}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		c := map[string]any{
			"start_page":  pr.StartPage,
			"end_page":    pr.EndPage,
			"section_ids": pr.SectionIDs,
		}

		if h.llm != nil {
			content := h.materialiseCitedContent(ctx, t, pr.SectionIDs)
			if strings.TrimSpace(content) != "" {
				ext := h.spanExtractor(requestModel)
				span, _, err := ext.Extract(ctx, content, query)
				if err == nil && span != nil && span.Text != "" {
					c["quote"] = span.Text
					if span.Start >= 0 && span.End > span.Start {
						c["quote_start"] = span.Start
						c["quote_end"] = span.End
					}
				}
			}
		}

		citations = append(citations, c)
	}

	// Stable sort by start_page so output ordering is deterministic
	// across runs that fetch the same pages in different orders.
	sort.SliceStable(citations, func(i, j int) bool {
		return citations[i]["start_page"].(int) < citations[j]["start_page"].(int)
	})

	return citations
}

// materialiseCitedContent loads + concatenates every cited section's
// content (capped at 16K chars), used for answer-span extraction over
// the pages the model relied on.
func (h *AnswerPageIndexHandler) materialiseCitedContent(ctx context.Context, t *tree.Tree, sectionIDs []tree.SectionID) string {
	if len(sectionIDs) == 0 {
		return ""
	}
	var (
		b      strings.Builder
		budget = 16000
	)
	for _, id := range sectionIDs {
		if b.Len() >= budget {
			break
		}
		sec := t.FindByID(id)
		if sec == nil || sec.ContentRef == "" {
			continue
		}
		rc, _, err := h.storage.Get(ctx, sec.ContentRef)
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(raw))
		remaining := budget - b.Len()
		if remaining <= 0 {
			break
		}
		if len(text) > remaining {
			text = text[:remaining]
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return b.String()
}

// spanExtractor builds a SpanExtractor for citation quoting, using the
// same model fall-through as the /v1/answer handler.
func (h *AnswerPageIndexHandler) spanExtractor(requestModel string) *retrieval.SpanExtractor {
	model := h.answerSpan.Model
	if model == "" {
		model = requestModel
	}
	if model == "" {
		model = h.llmModel
	}
	ext := retrieval.NewSpanExtractor(h.llm, model)
	if h.answerSpan.MaxQuoteLen > 0 {
		ext.MaxQuoteLen = h.answerSpan.MaxQuoteLen
	}
	return ext
}

// pageIndexEventToTraceMap converts a PageIndexEvent into the
// reasoning_trace entry shape. Only documented fields ship.
func pageIndexEventToTraceMap(ev retrieval.PageIndexEvent) map[string]any {
	args := map[string]any{}
	switch ev.Type {
	case "get_pages":
		if ev.StartPage > 0 {
			args["start_page"] = ev.StartPage
		}
		if ev.EndPage > 0 {
			args["end_page"] = ev.EndPage
		}
	case "done":
		if len(ev.CitedPages) > 0 {
			args["cited_pages"] = ev.CitedPages
		}
	}
	entry := map[string]any{
		"hop":  ev.Hop,
		"tool": ev.Type,
	}
	if len(args) > 0 {
		entry["args"] = args
	}
	if ev.Reasoning != "" {
		entry["reasoning"] = ev.Reasoning
	}
	if ev.Note != "" {
		entry["note"] = ev.Note
	}
	if ev.CharCount > 0 {
		entry["result_chars"] = ev.CharCount
	}
	if len(ev.SectionIDs) > 0 {
		entry["sections_touched"] = ev.SectionIDs
	}
	if ev.Answer != "" {
		entry["answer"] = ev.Answer
	}
	return entry
}

// scopedTOCProvider adapts the DB pool to retrieval.TOCProvider with a
// fixed org/store scope captured per request. It reads the persisted
// documents.toc_tree for the requesting tenant and returns it verbatim
// for the get_document_structure tool; a NULL column surfaces as
// retrieval.ErrNoTOC so the strategy degrades to its synthesised view.
type scopedTOCProvider struct {
	db      *db.Pool
	orgID   string
	storeID string
}

func (p scopedTOCProvider) GetTOC(ctx context.Context, docID tree.DocumentID) ([]byte, error) {
	doc, err := p.db.GetDocument(ctx, docID, p.orgID, p.storeID)
	if err != nil {
		return nil, err
	}
	if len(doc.TOCTree) == 0 {
		return nil, retrieval.ErrNoTOC
	}
	return doc.TOCTree, nil
}
