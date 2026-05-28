package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// AnswerHandler implements POST /v1/answer: retrieval + per-section
// answer-span extraction + a synthesis LLM call, returning a
// quote-grounded answer plus citations in one round-trip. Every
// citation carries a section ID, page range (when known), and the
// verbatim quote the answer relies on.
//
// Ported from cmd/engine's internal/api.handleAnswer, adapted to the
// deployed server's multi-tenant model: the org + store come from the
// X-Vectorless-Org / X-Vectorless-Store headers rather than the
// standalone nil-UUID org.
type AnswerHandler struct {
	logger     *slog.Logger
	db         *db.Pool
	storage    storage.Storage
	strategy   retrieval.Strategy
	llm        llmgate.Client
	llmModel   string
	answerSpan enginecfg.AnswerSpanBlock
	answer     enginecfg.AnswerBlock
	replay     retrieval.ReplayStore
}

// NewAnswerHandler creates an AnswerHandler. llm may be nil, in which
// case the endpoint returns 501; replay may be nil, which skips
// replay capture.
func NewAnswerHandler(
	logger *slog.Logger,
	pool *db.Pool,
	store storage.Storage,
	strategy retrieval.Strategy,
	llm llmgate.Client,
	llmModel string,
	answerSpan enginecfg.AnswerSpanBlock,
	answer enginecfg.AnswerBlock,
	replay retrieval.ReplayStore,
) *AnswerHandler {
	return &AnswerHandler{
		logger:     logger,
		db:         pool,
		storage:    store,
		strategy:   strategy,
		llm:        llm,
		llmModel:   llmModel,
		answerSpan: answerSpan,
		answer:     answer,
		replay:     replay,
	}
}

// answerRequest is the JSON body for POST /v1/answer.
type answerRequest struct {
	DocumentID        tree.DocumentID `json:"document_id"`
	Query             string          `json:"query"`
	Model             string          `json:"model"`
	MaxTokens         int             `json:"max_tokens"`
	ReservedForPrompt int             `json:"reserved_for_prompt"`
	MaxParallelCalls  int             `json:"max_parallel_calls"`
	MaxSections       int             `json:"max_sections"`
	MaxAnswerTokens   int             `json:"max_answer_tokens"`
}

// HandleAnswer runs retrieval, extracts a grounding quote per
// returned section, synthesises a final answer, and returns it with
// per-section citations.
func (h *AnswerHandler) HandleAnswer(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	if h.llm == nil {
		writeErr(w, http.StatusNotImplemented, "answer endpoint requires an LLM client")
		return
	}
	if h.strategy == nil {
		writeErr(w, http.StatusServiceUnavailable, "no retrieval strategy configured")
		return
	}

	var body answerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.DocumentID == "" || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_id and query are required")
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
	totalUsage := retrieval.Usage{}

	ids, retrievalUsage, err := h.runSelection(r.Context(), t, body.Query, budget)
	if err != nil {
		h.logger.Error("answer: strategy failed", "err", err, "document_id", body.DocumentID)
		writeErr(w, http.StatusInternalServerError, "retrieval failed: "+err.Error())
		return
	}
	totalUsage.Add(retrievalUsage)

	maxSections := body.MaxSections
	if maxSections <= 0 {
		maxSections = h.answer.MaxSections
	}
	if maxSections <= 0 {
		maxSections = 5
	}
	if len(ids) > maxSections {
		ids = ids[:maxSections]
	}

	// Load each section's content.
	enriched := make([]answerSection, 0, len(ids))
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
		enriched = append(enriched, answerSection{sec: sec, content: content})
	}

	// Always extract spans for /v1/answer — they ground each citation.
	spanExtractor := h.spanExtractor(body.Model)
	runAnswerSpansConcurrent(r.Context(), spanExtractor, body.Query, enriched, h.answerSpan.MaxConcurrency, h.logger)

	// Synthesise the final answer from the retrieved evidence.
	synthModel := h.answer.Model
	if synthModel == "" {
		synthModel = body.Model
	}
	if synthModel == "" {
		synthModel = h.llmModel
	}
	maxAnswerTokens := body.MaxAnswerTokens
	if maxAnswerTokens <= 0 {
		maxAnswerTokens = h.answer.MaxAnswerTokens
	}
	if maxAnswerTokens <= 0 {
		maxAnswerTokens = 1024
	}

	answerText, synthUsage, err := synthesiseAnswer(r.Context(), h.llm, synthModel, body.Query, enriched, maxAnswerTokens)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "synthesis failed: "+err.Error())
		return
	}
	totalUsage.Add(synthUsage)

	citations := make([]map[string]any, 0, len(enriched))
	finalIDs := make([]tree.SectionID, 0, len(enriched))
	for _, e := range enriched {
		finalIDs = append(finalIDs, e.sec.ID)
		c := map[string]any{
			"section_id": e.sec.ID,
			"title":      e.sec.Title,
		}
		if e.sec.PageStart > 0 {
			c["page_start"] = e.sec.PageStart
		}
		if e.sec.PageEnd > 0 {
			c["page_end"] = e.sec.PageEnd
		}
		if e.span != nil && e.span.Text != "" {
			c["quote"] = e.span.Text
			if e.span.Start >= 0 && e.span.End > e.span.Start {
				c["quote_start"] = e.span.Start
				c["quote_end"] = e.span.End
			}
		}
		citations = append(citations, c)
	}

	// Trace token hashes over the final IDs that ground the answer +
	// the synthesis model. Different synth models for the same
	// retrieval set produce different answers and therefore different
	// tokens.
	traceToken := retrieval.ComputeTraceToken(body.DocumentID, "1", synthModel, finalIDs)

	resp := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"answer":      answerText,
		"citations":   citations,
		"strategy":    h.strategy.Name(),
		"model":       synthModel,
		"usage": map[string]any{
			"input_tokens":  totalUsage.InputTokens,
			"output_tokens": totalUsage.OutputTokens,
			"total_tokens":  totalUsage.TotalTokens,
			"cost_usd":      totalUsage.CostUSD,
			"llm_calls":     totalUsage.LLMCalls,
		},
		"elapsed_ms":  time.Since(started).Milliseconds(),
		"trace_token": traceToken,
	}

	raw, err := marshalJSONForReplay(resp)
	if err != nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeJSONWithReplay(w, h.replay, http.StatusOK, raw, traceToken, retrieval.ReplayEntry{
		DocumentID:  body.DocumentID,
		Query:       body.Query,
		Model:       synthModel,
		SelectedIDs: finalIDs,
	})
}

// runSelection picks section IDs for the query, surfacing cost when
// the strategy implements CostStrategy.
func (h *AnswerHandler) runSelection(ctx context.Context, t *tree.Tree, query string, budget retrieval.ContextBudget) ([]tree.SectionID, retrieval.Usage, error) {
	if cs, ok := h.strategy.(retrieval.CostStrategy); ok {
		res, err := cs.SelectWithCost(ctx, t, query, budget)
		if err != nil {
			return nil, retrieval.Usage{}, err
		}
		if res == nil {
			return nil, retrieval.Usage{}, nil
		}
		return res.SelectedIDs, res.Usage, nil
	}
	ids, err := h.strategy.Select(ctx, t, query, budget)
	if err != nil {
		return nil, retrieval.Usage{}, err
	}
	return ids, retrieval.Usage{}, nil
}

// spanExtractor builds a SpanExtractor honouring the configured model
// override, with a fall-through to the request's model then the
// engine default.
func (h *AnswerHandler) spanExtractor(requestModel string) *retrieval.SpanExtractor {
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

// answerSection bundles a tree section with its loaded content and
// the extracted answer span. Shared by /v1/answer.
type answerSection struct {
	sec     *tree.Section
	content string
	span    *retrieval.AnswerSpan
}

// runAnswerSpansConcurrent fans out span extraction across secs with a
// max-concurrency semaphore. Each extraction's outcome is written back
// into the matching slot's span field. Errors are logged and dropped —
// span extraction is best-effort.
func runAnswerSpansConcurrent(ctx context.Context, extractor *retrieval.SpanExtractor, query string, secs []answerSection, maxConcurrency int, logger *slog.Logger) {
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for i := range secs {
		i := i
		if strings.TrimSpace(secs[i].content) == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			span, _, err := extractor.Extract(ctx, secs[i].content, query)
			if err != nil {
				if logger != nil {
					logger.Warn("answer-span: extract failed", "section_id", secs[i].sec.ID, "err", err)
				}
				return
			}
			secs[i].span = span
		}()
	}
	wg.Wait()
}

// synthesiseAnswer runs one LLM call producing the final answer from
// retrieved sections + their extracted spans. The model is told to
// cite by section ID.
func synthesiseAnswer(ctx context.Context, client llmgate.Client, model, query string, secs []answerSection, maxAnswerTokens int) (string, retrieval.Usage, error) {
	var b strings.Builder
	b.WriteString("You are answering a user's question using ONLY the evidence below.\n\n")
	b.WriteString("User query:\n")
	b.WriteString(query)
	b.WriteString("\n\nRetrieved evidence (each block is a section of the document):\n")
	for i, e := range secs {
		fmt.Fprintf(&b, "\n[%d] section_id=%s, title=%q", i+1, e.sec.ID, e.sec.Title)
		if e.sec.PageStart > 0 {
			fmt.Fprintf(&b, ", pages=%d-%d", e.sec.PageStart, e.sec.PageEnd)
		}
		b.WriteString("\n")
		if e.span != nil && e.span.Text != "" {
			fmt.Fprintf(&b, "Most relevant quote: %q\n", e.span.Text)
		}
		// Always include some content so the model isn't blind when the
		// span extractor returned nothing.
		if e.content != "" {
			snippet := e.content
			if len(snippet) > 4000 {
				snippet = snippet[:4000]
			}
			fmt.Fprintf(&b, "Section content:\n%s\n", snippet)
		}
	}
	b.WriteString("\nWrite a concise answer to the user's query. ")
	b.WriteString("If the evidence does not contain an answer, say so. ")
	b.WriteString("Inline citations should reference the section_id values shown above. ")
	b.WriteString("Output plain prose; no JSON.")

	req := llmgate.Request{
		Model: model,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: "You synthesise grounded answers from retrieved document sections. Never invent facts; only cite what the evidence shows."},
			{Role: llmgate.RoleUser, Content: b.String()},
		},
		MaxTokens:   maxAnswerTokens,
		Temperature: 0,
	}
	resp, err := client.Complete(ctx, req)
	if err != nil {
		return "", retrieval.Usage{}, err
	}
	return strings.TrimSpace(resp.Content), retrieval.Usage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		CostUSD:      resp.Usage.CostUSD,
		LLMCalls:     1,
	}, nil
}
