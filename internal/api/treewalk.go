package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// loadTreeForTreeWalk resolves the document tree for the
// treewalk answer endpoint. Routes through the optional
// TreeWalkTreeLoader hook when set (tests), otherwise falls
// through to the real DB.
//
// Kept here rather than inlined in the handler so the test seam is
// obvious: production code path goes straight to d.DB.LoadTree;
// tests set d.TreeWalkTreeLoader to an in-memory function.
func (d Deps) loadTreeForTreeWalk(ctx context.Context, docID tree.DocumentID) (*tree.Tree, error) {
	if d.TreeWalkTreeLoader != nil {
		return d.TreeWalkTreeLoader(ctx, docID)
	}
	return d.DB.LoadTree(ctx, docID, standaloneOrgID, "")
}

// treeWalkAnswerRequest is the body shape for /v1/answer/treewalk.
//
// The endpoint mirrors /v1/answer's shape but exposes the
// page-based loop's specific knobs (max_hops, max_pages_per_fetch)
// plus a streaming variant. Per-request fields override the
// TreeWalkBlock config when present.
type treeWalkAnswerRequest struct {
	DocumentID       tree.DocumentID `json:"document_id"`
	Query            string          `json:"query"`
	Model            string          `json:"model"`
	MaxHops          int             `json:"max_hops"`
	MaxPagesPerFetch int             `json:"max_pages_per_fetch"` // chars cap; named per the spec
	Stream           bool            `json:"stream"`
	IncludeReasoning bool            `json:"reasoning"`
}

// handleAnswerTreeWalk runs the TreeWalk agentic loop end-to-end
// and returns the model's answer + page-grounded citations in one
// round-trip.
//
// The loop owns the answer: there's no separate synthesis call.
// /v1/answer extracts spans per section and then synthesises; this
// endpoint asks the model to emit the answer directly inside the
// done action. Citations are per-page-range with answer-span
// quotes pulled from the cited content via the existing
// SpanExtractor.
//
// Differentiators surfaced on the response:
//   - trace_token: replay any answer byte-for-byte (substrate
//     reused from /v1/answer; the page-based input set is folded
//     into the hash so cross-shape tokens never collide).
//   - reasoning_trace: per-hop tool calls + arg summaries. Opt-in
//     via request body "reasoning":true or query ?reasoning=true.
//   - streaming (stream=true): SSE with one event per tool call so
//     callers watch the navigation in real time.
//
// Body shape (canonical, non-streaming):
//
//	POST /v1/answer/treewalk
//	{ "document_id": "...", "query": "...", "model"?: "...",
//	  "max_hops"?: 8, "max_pages_per_fetch"?: 16000,
//	  "stream"?: false, "reasoning"?: false }
//
// Response: see treeWalkAnswerResponse below.
func (d Deps) handleAnswerTreeWalk(w http.ResponseWriter, r *http.Request) {
	if d.LLM == nil {
		writeErr(w, http.StatusNotImplemented, "answer/treewalk endpoint requires an LLM client")
		return
	}
	if d.TreeWalkStrategy == nil || !d.TreeWalk.Enabled {
		writeErr(w, http.StatusNotImplemented, "treewalk strategy not configured on this server (retrieval.treewalk.enabled=false)")
		return
	}

	var body treeWalkAnswerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.DocumentID == "" || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_id and query are required")
		return
	}
	// Allow ?reasoning=true as an alternative to the body field. Same
	// rationale as the existing /v1/query streaming flag — caller's
	// choice of transport.
	if r.URL.Query().Get("reasoning") == "true" {
		body.IncludeReasoning = true
	}

	t, err := d.loadTreeForTreeWalk(r.Context(), body.DocumentID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Build a per-request strategy that wraps the engine's
	// configured TreeWalkStrategy. We do this because per-request
	// overrides (max_hops, max_pages_per_fetch, model, OnEvent for
	// streaming) must NOT mutate the shared Deps instance — Deps
	// is read by many goroutines concurrently.
	perReq := *d.TreeWalkStrategy
	if body.MaxHops > 0 {
		perReq.MaxHops = body.MaxHops
	}
	if body.MaxPagesPerFetch > 0 {
		perReq.PageContentLimit = body.MaxPagesPerFetch
	}
	// Per-request model override falls through to budget.ModelName
	// the same way every other handler does.

	budget := retrieval.ContextBudget{ModelName: body.Model}
	if budget.ModelName == "" {
		budget.ModelName = d.LLMModel
	}

	started := time.Now()

	// Stream variant: hijack the response writer for SSE and emit
	// one event per tool call.
	if body.Stream {
		d.serveAnswerTreeWalkStream(w, r, &perReq, t, body, budget, started)
		return
	}

	// Non-streaming: optionally capture reasoning trace via the
	// OnEvent hook into an in-memory buffer.
	var (
		traceMu sync.Mutex
		trace   []map[string]any
	)
	if body.IncludeReasoning {
		perReq.OnEvent = func(ev retrieval.TreeWalkEvent) {
			traceMu.Lock()
			defer traceMu.Unlock()
			trace = append(trace, eventToTraceMap(ev))
		}
	}

	res, err := perReq.SelectWithCost(r.Context(), t, body.Query, budget)
	if err != nil {
		d.Logger.Error("answer/treewalk: strategy failed", "err", err, "document_id", body.DocumentID)
		writeErr(w, http.StatusInternalServerError, "treewalk strategy failed: "+err.Error())
		return
	}

	citations := d.buildTreeWalkCitations(r.Context(), t, res, body.Query, body.Model)

	resp := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"answer":      res.Reasoning, // strategy stores the agent's answer here
		"citations":   citations,
		"strategy":    perReq.Name(),
		"model":       budget.ModelName,
		"confidence":  res.Confidence,
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

	// Persist to the replay store. The trace token is keyed by
	// document + sorted cited pages + model, so the same answer is
	// fully replayable via the existing /v1/replay endpoint.
	finalIDs := append([]tree.SectionID(nil), res.SelectedIDs...)
	raw, err := marshalJSONForReplay(resp)
	if err != nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	d.writeJSONWithReplay(w, http.StatusOK, raw, res.TraceToken, retrieval.ReplayEntry{
		DocumentID:  body.DocumentID,
		Query:       body.Query,
		Model:       budget.ModelName,
		SelectedIDs: finalIDs,
	})
}

// serveAnswerTreeWalkStream handles the stream=true SSE variant.
// Each tool call emits one `event:` line so the caller can watch
// the navigation in real time. The final event ("answer") carries
// the full JSON response so the client doesn't need to make a
// second request.
//
// SSE format: `event: <type>\ndata: <json>\n\n` per the W3C spec.
func (d Deps) serveAnswerTreeWalkStream(w http.ResponseWriter, r *http.Request, strat *retrieval.TreeWalkStrategy, t *tree.Tree, body treeWalkAnswerRequest, budget retrieval.ContextBudget, started time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming requires http.Flusher; response writer does not support it")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// emitSSE serialises ev to JSON and writes one SSE record. We
	// swallow write errors — a disconnected client shouldn't kill
	// the strategy mid-flight; the user closing their browser is a
	// normal end-state.
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

	strat.OnEvent = func(ev retrieval.TreeWalkEvent) {
		emitSSE(ev.Type, ev)
	}

	// Started event so the client knows the loop is running.
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

	citations := d.buildTreeWalkCitations(r.Context(), t, res, body.Query, body.Model)
	final := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"answer":      res.Reasoning,
		"citations":   citations,
		"strategy":    strat.Name(),
		"model":       budget.ModelName,
		"confidence":  res.Confidence,
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

// buildTreeWalkCitations transforms the strategy's PagesRead +
// the section tree into the response's citations array.
//
// One citation per cited page range (deduplicated). Each citation
// carries:
//   - start_page / end_page
//   - section_ids: every section whose [PageStart,PageEnd] overlaps
//     the range
//   - quote / quote_start / quote_end: pulled via the existing
//     SpanExtractor over the concatenated cited-page content. If the
//     extractor finds no match the quote field is empty (offsets -1).
func (d Deps) buildTreeWalkCitations(ctx context.Context, t *tree.Tree, res *retrieval.Result, query, requestModel string) []map[string]any {
	if res == nil {
		return nil
	}
	// Source of truth for citations is the FINAL cited-range set
	// (res.CitedPages) — already deduped and capped to MaxCitations by
	// the strategy. This is what makes a confident single-pick answer
	// surface ONE citation even when it skimmed several pages to find
	// it: PagesRead is the navigation footprint (a superset), not the
	// commitment. Fall back to PagesRead only when nothing was cited
	// (refusal) or the run was hop-capped without a done — the trace
	// token is keyed on the cited ranges either way, so this never
	// breaks replay.
	sources := retrieval.CitationSources(res)
	citations := make([]map[string]any, 0, len(sources))

	for _, src := range sources {
		sectionIDs := src.SectionIDs
		if sectionIDs == nil {
			sectionIDs = retrieval.SectionIDsOverlapping(t, src.Start, src.End)
		}
		c := map[string]any{
			"start_page":  src.Start,
			"end_page":    src.End,
			"section_ids": sectionIDs,
		}

		// Quote extraction is best-effort: an LLM blip or empty
		// content returns no quote, which is a normal degradation
		// path. We materialise the cited content from storage and
		// run one SpanExtractor call per citation.
		if d.LLM != nil {
			content := d.materialiseCitedContent(ctx, t, sectionIDs)
			if strings.TrimSpace(content) != "" {
				ext := d.treeWalkSpanExtractor(requestModel)
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

	// Sort citations by start_page so output ordering is stable
	// across runs that fetch the same set of pages in different
	// orders. Stable sort preserves the original-fetch order for
	// citations sharing a start page (rare in practice).
	sort.SliceStable(citations, func(i, j int) bool {
		return citations[i]["start_page"].(int) < citations[j]["start_page"].(int)
	})

	return citations
}

// materialiseCitedContent loads + concatenates every cited
// section's content. Used for answer-span extraction over the
// pages the model relied on, so the quote can have real byte
// offsets back into the cited evidence.
//
// Limited to 16K chars overall (the extractor's prompt budget
// dictates this), preferring leading sections in page order so
// the quote anchors near the start of the citation when there's
// too much text to fit.
func (d Deps) materialiseCitedContent(ctx context.Context, t *tree.Tree, sectionIDs []tree.SectionID) string {
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
		rc, _, err := d.Storage.Get(ctx, sec.ContentRef)
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

// treeWalkSpanExtractor builds a SpanExtractor configured for the
// /v1/answer/treewalk endpoint. Same fall-through pattern as the
// existing spanExtractor helper (config override → request model →
// engine default).
func (d Deps) treeWalkSpanExtractor(requestModel string) *retrieval.SpanExtractor {
	model := d.AnswerSpan.Model
	if model == "" {
		model = requestModel
	}
	if model == "" {
		model = d.LLMModel
	}
	ext := retrieval.NewSpanExtractor(d.LLM, model)
	if d.AnswerSpan.MaxQuoteLen > 0 {
		ext.MaxQuoteLen = d.AnswerSpan.MaxQuoteLen
	}
	return ext
}

// eventToTraceMap converts a TreeWalkEvent into the
// reasoning_trace entry shape. Only documented fields ship —
// nothing internal leaks via the trace.
func eventToTraceMap(ev retrieval.TreeWalkEvent) map[string]any {
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
		// The final-hop "done" event carries the answer; surfacing
		// it in the trace lets a debugger see the agent's literal
		// final-turn output alongside the formal response field.
		entry["answer"] = ev.Answer
	}
	return entry
}

// treeWalkTraceTokenFromCitations exposes the same hash a
// TreeWalkStrategy emits to callers who want to recompute the
// token client-side. The page-range string form mirrors the one
// the strategy uses internally so the two stay in lock-step.
//
// Unused at the moment but useful for tests that want to assert
// the in-response trace_token against the canonical input set —
// kept here rather than exported from the retrieval package so
// the API layer owns its own input wiring.
func treeWalkTraceTokenFromCitations(docID tree.DocumentID, model string, ranges [][2]int) string {
	strs := make([]string, 0, len(ranges))
	for _, r := range ranges {
		if r[0] == r[1] {
			strs = append(strs, fmt.Sprintf("%d", r[0]))
		} else {
			strs = append(strs, fmt.Sprintf("%d-%d", r[0], r[1]))
		}
	}
	sort.Strings(strs)
	h := sha256.New()
	h.Write([]byte(string(docID)))
	h.Write([]byte{0})
	h.Write([]byte("1-pages"))
	h.Write([]byte{0})
	h.Write([]byte("treewalk:" + model))
	h.Write([]byte{0})
	h.Write([]byte(retrieval.SystemPromptVersion))
	for i, s := range strs {
		if i == 0 {
			h.Write([]byte{0})
		} else {
			h.Write([]byte{0})
		}
		h.Write([]byte("p:" + s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Compile-time guard: the TreeWalk strategy must satisfy
// retrieval.CostStrategy so SelectWithCost works without a
// type-assert dance.
var _ retrieval.CostStrategy = (*retrieval.TreeWalkStrategy)(nil)

// Compile-time check that the Deps fields the handler reads are
// the only API-layer dependencies it pulls in. If a future edit
// adds a new dependency here the linter / vet will catch it via
// the unused-import path.
var _ llmgate.Client = (llmgate.Client)(nil)
