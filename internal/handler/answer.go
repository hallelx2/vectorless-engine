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
	"github.com/hallelx2/vectorless-engine/pkg/pincite"
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

	// judge chooses each pincite's sentence; pincites serves page
	// layouts. Both optional — see WithPincites.
	judge    llmgate.Judge
	pincites *pincite.Service

	byokFactory LLMFactory
}

// WithPincites enables page-level pincites on answers built from
// evidence pages. judge may be nil (sentences are then chosen
// lexically); svc may be nil (citations then carry no regions).
func (h *AnswerHandler) WithPincites(judge llmgate.Judge, svc *pincite.Service) *AnswerHandler {
	h.judge = judge
	h.pincites = svc
	return h
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
	// Stream answers as Server-Sent Events; ?stream=true also works.
	Stream bool `json:"stream"`

	// byok is the caller's own model for the answer step, from the
	// X-LLM-* headers (see resolveBYOK). Never serialised.
	byok *byokClient
}

// byokClient is a per-request model the caller brought.
type byokClient struct {
	client   llmgate.Client
	model    string
	provider string
}

// LLMFactory builds a client for a caller-supplied provider and key.
type LLMFactory func(provider, apiKey, model string) (llmgate.Client, string, error)

// WithBYOK lets callers answer on their own model: X-LLM-Provider
// (anthropic | openai | gemini), X-LLM-Api-Key and optionally
// X-LLM-Model. Retrieval is unaffected; only the one generative call —
// writing the answer — runs on the caller's key.
func (h *AnswerHandler) WithBYOK(f LLMFactory) *AnswerHandler {
	h.byokFactory = f
	return h
}

// resolveBYOK reads the X-LLM-* headers. ok is false (and the response
// written) when they are present but unusable.
func (h *AnswerHandler) resolveBYOK(w http.ResponseWriter, r *http.Request) (*byokClient, bool) {
	key := r.Header.Get("X-LLM-Api-Key")
	if key == "" {
		return nil, true
	}
	if h.byokFactory == nil {
		writeErr(w, http.StatusNotImplemented, "this server does not accept caller-supplied model keys")
		return nil, false
	}
	provider := strings.ToLower(strings.TrimSpace(r.Header.Get("X-LLM-Provider")))
	c, model, err := h.byokFactory(provider, key, r.Header.Get("X-LLM-Model"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "model key: "+err.Error())
		return nil, false
	}
	return &byokClient{client: c, model: model, provider: provider}, true
}

func (b *byokClient) or(c llmgate.Client, model string) (llmgate.Client, string) {
	if b == nil {
		return c, model
	}
	return b.client, b.model
}

// HandleAnswer runs retrieval, then answers from what it found.
//
// When the strategy returns evidence pages (judgewalk), the answer is
// written from those pages with an inline [n] marker on every claim,
// and each marker becomes a pincite: the sentence on the cited page
// that states the claim, chosen by the Judge, located on the page and
// returned with its regions (HAL-837). Otherwise it is the section
// path: a grounding quote per selected section and one synthesis call.
//
// stream=true (body or query) answers as Server-Sent Events: "started",
// "retrieved" once the evidence is chosen — before the one generative
// call — and "answer" carrying the full response.
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
	if r.URL.Query().Get("stream") == "true" {
		body.Stream = true
	}
	byok, ok := h.resolveBYOK(w, r)
	if !ok {
		return
	}
	body.byok = byok

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

	// emit is a no-op unless streaming; fail reports an error on
	// whichever channel the caller is reading.
	emit := func(string, any) {}
	fail := func(status int, msg string) { writeErr(w, status, msg) }
	if body.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeErr(w, http.StatusInternalServerError, "streaming requires http.Flusher; response writer does not support it")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		declareTokenTrailers(w.Header())
		w.WriteHeader(http.StatusOK)
		var mu sync.Mutex
		emit = func(event string, payload any) {
			raw, err := json.Marshal(payload)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
			flusher.Flush()
		}
		fail = func(_ int, msg string) { emit("error", map[string]string{"error": msg}) }
		emit("started", map[string]any{
			"document_id": body.DocumentID,
			"query":       body.Query,
			"strategy":    h.strategy.Name(),
		})
	}

	// Streaming callers see each navigation stage as it completes.
	strategy := h.strategy
	if ss, ok := strategy.(retrieval.StepStrategy); ok && body.Stream {
		stepStart := time.Now()
		strategy = ss.WithSteps(func(st retrieval.NavStep) {
			emit("step", map[string]any{"step": st, "elapsed_ms": time.Since(stepStart).Milliseconds()})
		})
	}
	res, err := h.runSelection(r.Context(), strategy, t, body.Query, budget)
	if err != nil {
		h.logger.Error("answer: strategy failed", "err", err, "document_id", body.DocumentID)
		fail(http.StatusInternalServerError, retrieval.PublicError(err))
		return
	}

	var (
		resp     map[string]any
		finalIDs []tree.SectionID
		model    string
		token    string
	)
	if len(res.EvidencePages) > 0 {
		src, serr := pinciteSource(r.Context(), h.db, body.DocumentID, orgID, storeID(r))
		if serr != nil {
			h.logger.Warn("answer: document source unavailable; pincites degrade", "document_id", body.DocumentID, "err", serr)
		}
		resp, finalIDs, model, token, err = h.answerFromPages(r.Context(), t, src, body, res, started, emit)
	} else {
		resp, finalIDs, model, token, err = h.answerFromSections(r.Context(), t, body, res, started)
	}
	if err != nil {
		h.logger.Error("answer: generation failed", "err", err, "document_id", body.DocumentID)
		fail(http.StatusInternalServerError, retrieval.PublicError(err))
		return
	}

	raw, err := marshalJSONForReplay(resp)
	entry := retrieval.ReplayEntry{DocumentID: body.DocumentID, Query: body.Query, Model: model, SelectedIDs: finalIDs}
	if body.Stream {
		emit("answer", resp)
		setTokenHeaders(w.Header(), resp)
		if err == nil && h.replay != nil && token != "" {
			entry.ResponseJSON = raw
			entry.CreatedAt = time.Now()
			h.replay.Put(token, entry)
		}
		return
	}
	setTokenHeaders(w.Header(), resp)
	if err != nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeJSONWithReplay(w, h.replay, http.StatusOK, raw, token, entry)
}

// answerFromSections is the section path: a grounding quote per
// selected section, one synthesis call, one citation per section.
func (h *AnswerHandler) answerFromSections(ctx context.Context, t *tree.Tree, body answerRequest, res *retrieval.Result, started time.Time) (map[string]any, []tree.SectionID, string, string, error) {
	totalUsage := retrieval.Usage{}
	totalUsage.Add(res.Usage)
	ids := res.SelectedIDs

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

	enriched := make([]answerSection, 0, len(ids))
	for _, id := range ids {
		sec := t.FindByID(id)
		if sec == nil {
			continue
		}
		var content string
		if sec.ContentRef != "" {
			rc, _, getErr := h.storage.Get(ctx, sec.ContentRef)
			if getErr == nil {
				raw, _ := io.ReadAll(rc)
				_ = rc.Close() // best-effort close
				content = string(raw)
			}
		}
		enriched = append(enriched, answerSection{sec: sec, content: content})
	}

	spanExtractor := h.spanExtractor(body.Model)
	runAnswerSpansConcurrent(ctx, spanExtractor, body.Query, enriched, h.answerSpan.MaxConcurrency, h.logger)

	client, synthModel := body.byok.or(h.llm, h.synthModel(body.Model))
	answerText, synthUsage, err := synthesiseAnswer(ctx, client, synthModel, body.Query, enriched, h.maxAnswerTokens(body, 1024))
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("synthesis failed: %w", err)
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

	// Trace token covers the FINAL citation IDs (post-maxSections cap)
	// and the synthesis model, so two calls that cite identical
	// sections under identical models share a token.
	traceToken := retrieval.ComputeTraceToken(body.DocumentID, "1", synthModel, finalIDs)

	resp := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"answer":      answerText,
		"citations":   citations,
		"strategy":    h.strategy.Name(),
		"model":       synthModel,
		"usage":       usageMap(totalUsage),
		"elapsed_ms":  time.Since(started).Milliseconds(),
		"trace_token": traceToken,
	}
	return resp, finalIDs, synthModel, traceToken, nil
}

func (h *AnswerHandler) synthModel(requestModel string) string {
	m := h.answer.Model
	if m == "" {
		m = requestModel
	}
	if m == "" {
		m = h.llmModel
	}
	return m
}

func (h *AnswerHandler) maxAnswerTokens(body answerRequest, fallback int) int {
	n := body.MaxAnswerTokens
	if n <= 0 {
		n = h.answer.MaxAnswerTokens
	}
	if n <= 0 {
		n = fallback
	}
	return n
}

// runSelection runs the strategy, surfacing cost and evidence pages
// when it implements CostStrategy.
func (h *AnswerHandler) runSelection(ctx context.Context, strategy retrieval.Strategy, t *tree.Tree, query string, budget retrieval.ContextBudget) (*retrieval.Result, error) {
	if cs, ok := strategy.(retrieval.CostStrategy); ok {
		res, err := cs.SelectWithCost(ctx, t, query, budget)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return &retrieval.Result{}, nil
		}
		return res, nil
	}
	ids, err := strategy.Select(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return &retrieval.Result{SelectedIDs: ids}, nil
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
		Temperature: llmgate.Float64(0),
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
