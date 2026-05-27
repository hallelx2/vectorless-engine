// Package api exposes the engine's v1 HTTP API.
//
// Routes are versioned under /v1 from day one. Breaking changes will ship
// under /v2 and run alongside /v1 through a deprecation window.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/config"
	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/ingest"
	"github.com/hallelx2/vectorless-engine/pkg/queue"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// standaloneOrgID is the canonical org identifier the standalone
// engine binary (cmd/engine) uses for every document it manages.
// Self-hosted engine deployments are single-tenant by design — the
// nil UUID gives us a stable, never-real-user "org" so the same
// org-scoped DB methods can be reused without duplicating logic.
const standaloneOrgID = "00000000-0000-0000-0000-000000000000"

// Deps bundles the engine's subsystems for injection into the API layer.
type Deps struct {
	Logger   *slog.Logger
	DB       *db.Pool
	Storage  storage.Storage
	Queue    queue.Queue
	Strategy retrieval.Strategy
	Version  string

	// MultiDoc is the multi-document query dispatcher. If nil, the
	// /v1/query/multi endpoint returns 501.
	MultiDoc *retrieval.MultiDoc

	// LLM is the shared llmgate client used by handlers that issue
	// LLM calls outside the retrieval strategy (answer-span extraction,
	// /v1/answer synthesis). Nil disables those handlers (the endpoints
	// return 501).
	LLM llmgate.Client

	// LLMModel is the default model name. Per-request overrides win.
	LLMModel string

	// AnswerSpan / Answer hold the relevant config blocks. Default
	// values (AnswerSpan disabled, Answer.MaxSections=5) are safe.
	AnswerSpan config.AnswerSpanBlock
	Answer     config.AnswerBlock

	// Planner runs one LLM call before retrieval to build a structured
	// Plan (intent + entities + multi-hop sub-questions). Nil disables
	// planning even when a request opts in via `enable_planning`.
	Planner *retrieval.Planner

	// Planning carries the server-side planning config. The body-level
	// `enable_planning` field on /v1/query and /v1/answer overrides
	// Planning.Enabled.
	Planning config.PlanningBlock

	// ReRanker runs Phase 2.3 content-aware re-rank on the strategy's
	// candidate sections (one extra LLM call per query). Nil disables
	// re-rank even when a request opts in via `enable_rerank`.
	ReRanker *retrieval.ReRanker

	// ReRank carries the server-side re-rank config. The body-level
	// `enable_rerank` field on /v1/query and /v1/answer overrides
	// ReRank.Enabled. TopK truncates the post-rerank candidate list.
	ReRank config.ReRankBlock
}

// Router builds and returns the chi router wired with v1 routes.
func Router(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Heartbeat("/health"))

	r.Route("/v1", func(r chi.Router) {
		r.Get("/health", d.handleHealth)
		r.Get("/version", d.handleVersion)

		r.Route("/documents", func(r chi.Router) {
			r.Get("/", d.handleListDocuments)
			r.Post("/", d.handleIngestDocument)
			r.Get("/{id}", d.handleGetDocument)
			r.Delete("/{id}", d.handleDeleteDocument)
			r.Get("/{id}/tree", d.handleGetTree)
		})

		r.Get("/sections/{id}", d.handleGetSection)
		r.Post("/query", d.handleQuery)
		r.Post("/query/multi", d.handleQueryMulti)
		r.Post("/answer", d.handleAnswer)
	})

	r.Post("/internal/jobs/{kind}", d.handleQueueWebhook)

	return r
}

func (d Deps) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (d Deps) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": d.Version})
}

// --- ingest / documents ---

// handleListDocuments returns a page of documents, most-recent first.
// Query params: limit (1..200, default 50), status, cursor (RFC3339
// created_at from the previous page's next_cursor).
func (d Deps) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Standalone single-tenant deployment uses the nil-UUID org so
	// reads + writes consistently land in one logical tenant. The
	// multi-tenant SaaS surface lives in vectorless-server and reads
	// X-Vectorless-Org instead.
	opts := db.ListDocumentsOpts{
		OrgID:  standaloneOrgID,
		Status: db.DocumentStatus(q.Get("status")),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.Cursor = t
		}
	}

	docs, next, err := d.DB.ListDocuments(r.Context(), opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		items = append(items, map[string]any{
			"id":           doc.ID,
			"title":        doc.Title,
			"content_type": doc.ContentType,
			"status":       string(doc.Status),
			"byte_size":    doc.ByteSize,
			"created_at":   doc.CreatedAt,
			"updated_at":   doc.UpdatedAt,
		})
	}
	resp := map[string]any{"items": items}
	if !next.IsZero() {
		resp["next_cursor"] = next.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleIngestDocument accepts a document via either multipart/form-data
// (field name: "file") or a JSON body { "content": "...", "filename": "..." }.
// The bytes are streamed to Storage, a documents row is created in
// "pending" state, and an ingest job is enqueued.
func (d Deps) handleIngestDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	docID := ingest.NewDocumentID()

	var (
		filename    string
		contentType string
		body        io.Reader
		size        int64
	)

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid multipart body: "+err.Error())
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, `missing form field "file"`)
			return
		}
		defer file.Close()
		filename = header.Filename
		contentType = header.Header.Get("Content-Type")
		body = file
		size = header.Size

	case strings.HasPrefix(ct, "application/json"):
		var payload struct {
			Filename    string `json:"filename"`
			ContentType string `json:"content_type"`
			Content     string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		if payload.Content == "" {
			writeErr(w, http.StatusBadRequest, `"content" is required`)
			return
		}
		filename = payload.Filename
		contentType = payload.ContentType
		body = strings.NewReader(payload.Content)
		size = int64(len(payload.Content))

	default:
		writeErr(w, http.StatusUnsupportedMediaType,
			"use multipart/form-data (file) or application/json (content)")
		return
	}

	if contentType == "" {
		contentType = guessContentType(filename)
	}

	key := ingest.SourceKey(docID, filename)
	if err := d.Storage.Put(ctx, key, body, storage.Metadata{
		Key: key, Size: size, ContentType: contentType,
	}); err != nil {
		d.Logger.Error("ingest: storage put failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "storage write failed")
		return
	}

	title := filename
	if title == "" {
		title = string(docID)
	}

	if err := d.DB.NewDocument(ctx, db.Document{
		ID:          docID,
		OrgID:       standaloneOrgID,
		Title:       title,
		ContentType: contentType,
		SourceRef:   key,
		Status:      db.StatusPending,
		ByteSize:    size,
	}); err != nil {
		d.Logger.Error("ingest: db insert failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "db write failed")
		return
	}

	payload, _ := json.Marshal(ingest.Payload{
		DocumentID:  docID,
		ContentType: contentType,
		Filename:    filename,
		SourceRef:   key,
	})
	if err := d.Queue.Enqueue(ctx, queue.Job{
		Kind:      queue.KindIngestDocument,
		Payload:   payload,
		DedupeKey: string(docID),
	}); err != nil {
		d.Logger.Error("ingest: enqueue failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "enqueue failed")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"document_id": docID,
		"status":      string(db.StatusPending),
	})
}

func (d Deps) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	id := tree.DocumentID(chi.URLParam(r, "id"))
	doc, err := d.DB.GetDocument(r.Context(), id, standaloneOrgID, "")
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            doc.ID,
		"title":         doc.Title,
		"content_type":  doc.ContentType,
		"status":        string(doc.Status),
		"byte_size":     doc.ByteSize,
		"error_message": doc.ErrorMessage,
		"metadata":      doc.Metadata,
		"created_at":    doc.CreatedAt,
		"updated_at":    doc.UpdatedAt,
	})
}

func (d Deps) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id := tree.DocumentID(chi.URLParam(r, "id"))
	if err := d.DB.DeleteDocument(r.Context(), id, standaloneOrgID, ""); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleGetTree(w http.ResponseWriter, r *http.Request) {
	id := tree.DocumentID(chi.URLParam(r, "id"))
	t, err := d.DB.LoadTree(r.Context(), id, standaloneOrgID, "")
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t.BuildView())
}

func (d Deps) handleGetSection(w http.ResponseWriter, r *http.Request) {
	id := tree.SectionID(chi.URLParam(r, "id"))
	sec, err := d.DB.GetSection(r.Context(), id, standaloneOrgID, "")
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "section not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var content string
	if sec.ContentRef != "" {
		rc, _, err := d.Storage.Get(r.Context(), sec.ContentRef)
		if err == nil {
			raw, _ := io.ReadAll(rc)
			rc.Close()
			content = string(raw)
		}
	}

	resp := map[string]any{
		"id":          sec.ID,
		"document_id": sec.DocumentID,
		"parent_id":   sec.ParentID,
		"ordinal":     sec.Ordinal,
		"depth":       sec.Depth,
		"title":       sec.Title,
		"summary":     sec.Summary,
		"token_count": sec.TokenCount,
		"metadata":    sec.Metadata,
		"content":     content,
	}
	if sec.PageStart > 0 {
		resp["page_start"] = sec.PageStart
	}
	if sec.PageEnd > 0 {
		resp["page_end"] = sec.PageEnd
	}
	if len(sec.CandidateQuestions) > 0 {
		resp["candidate_questions"] = sec.CandidateQuestions
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- query ---

// handleQuery accepts { document_id, query, model?, max_tokens?,
// reserved_for_prompt?, max_parallel_calls?, max_sections?,
// enable_planning? } and runs the configured retrieval.Strategy against
// the document's tree.
//
// When `enable_planning` is true (or `retrieval.planning.enabled` is on
// at config level) the request first issues a planning LLM call. The
// resulting Plan is surfaced in the response under "plan". If the plan
// is multi-hop and decomposition is enabled, retrieval fans out one
// strategy call per sub-question and unions the results.
func (d Deps) handleQuery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DocumentID        tree.DocumentID `json:"document_id"`
		Query             string          `json:"query"`
		Model             string          `json:"model"`
		MaxTokens         int             `json:"max_tokens"`
		ReservedForPrompt int             `json:"reserved_for_prompt"`
		MaxParallelCalls  int             `json:"max_parallel_calls"`
		MaxSections       int             `json:"max_sections"`
		// EnablePlanning opts this request into the Phase 2.1 query
		// planner. A pointer so we can distinguish "absent" from
		// "explicit false" — absent falls back to the server config.
		EnablePlanning *bool `json:"enable_planning"`
		// EnableReRank opts this request into the Phase 2.3
		// content-aware re-rank pass. Pointer for the same reason as
		// EnablePlanning. Overrides retrieval.rerank.enabled.
		EnableReRank *bool `json:"enable_rerank"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.DocumentID == "" || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_id and query are required")
		return
	}
	if d.Strategy == nil {
		writeErr(w, http.StatusServiceUnavailable, "no retrieval strategy configured")
		return
	}

	t, err := d.DB.LoadTree(r.Context(), body.DocumentID, standaloneOrgID, "")
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

	plan, _ := d.runPlanner(r.Context(), body.Query, body.EnablePlanning)
	ids, err := d.runSelection(r.Context(), t, plan, body.Query, budget)
	if err != nil {
		d.Logger.Error("query: strategy failed", "err", err, "document_id", body.DocumentID)
		writeErr(w, http.StatusInternalServerError, "retrieval failed: "+err.Error())
		return
	}
	if body.MaxSections > 0 && len(ids) > body.MaxSections {
		ids = ids[:body.MaxSections]
	}

	enriched := make([]sectionWithContent, 0, len(ids))
	for _, id := range ids {
		sec := t.FindByID(id)
		if sec == nil {
			continue
		}
		var content string
		if sec.ContentRef != "" {
			rc, _, err := d.Storage.Get(r.Context(), sec.ContentRef)
			if err == nil {
				raw, _ := io.ReadAll(rc)
				rc.Close()
				content = string(raw)
			}
		}
		enriched = append(enriched, sectionWithContent{sec: sec, content: content})
	}

	// Optional: content-aware re-rank pass. One LLM call that scores
	// each loaded section against the query and re-orders the slice
	// descending by score. TopK truncates the survivors. Failures
	// never drop sections — at worst the strategy's order is
	// preserved (see retrieval.ReRanker.ReRank).
	if d.reRankEnabled(body.EnableReRank) {
		enriched, _ = d.runReRank(r.Context(), enriched, body.Query, body.Model)
	}

	// Optional: per-section answer-span extraction. Opt-in via config —
	// one LLM call per returned section. Failures are non-fatal; the
	// section is returned without a span.
	if d.AnswerSpan.Enabled && d.LLM != nil {
		extractor := d.spanExtractor(body.Model)
		runSpansConcurrent(r.Context(), extractor, body.Query, enriched, d.AnswerSpan.MaxConcurrency, d.Logger)
	}

	sections := make([]map[string]any, 0, len(enriched))
	for _, e := range enriched {
		sections = append(sections, sectionWithContentToMap(e))
	}

	resp := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"strategy":    d.Strategy.Name(),
		"model":       body.Model,
		"sections":    sections,
		"elapsed_ms":  time.Since(started).Milliseconds(),
	}
	if plan != nil {
		resp["plan"] = plan
	}
	writeJSON(w, http.StatusOK, resp)
}

// sectionWithContent bundles a tree section with its loaded content
// and optional re-rank score / answer-span. Used by /v1/query and
// /v1/answer.
type sectionWithContent struct {
	sec     *tree.Section
	content string
	span    *retrieval.AnswerSpan

	// hasScore reports whether score was populated by a re-rank pass.
	// Distinct from score == 0 since 0 is a legitimate score the
	// model can return.
	hasScore bool
	score    float64
}

// sectionWithContentToMap renders the section as the API map shape.
func sectionWithContentToMap(e sectionWithContent) map[string]any {
	s := map[string]any{
		"id":          e.sec.ID,
		"parent_id":   e.sec.ParentID,
		"title":       e.sec.Title,
		"summary":     e.sec.Summary,
		"token_count": e.sec.TokenCount,
		"content":     e.content,
	}
	if e.sec.PageStart > 0 {
		s["page_start"] = e.sec.PageStart
	}
	if e.sec.PageEnd > 0 {
		s["page_end"] = e.sec.PageEnd
	}
	if len(e.sec.CandidateQuestions) > 0 {
		s["candidate_questions"] = e.sec.CandidateQuestions
	}
	if e.span != nil {
		s["answer_span"] = e.span
	}
	if e.hasScore {
		s["score"] = e.score
	}
	return s
}

// spanExtractor builds a SpanExtractor honouring the configured model
// override, with a fall-through to the request's model then Deps default.
func (d Deps) spanExtractor(requestModel string) *retrieval.SpanExtractor {
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

// runSpansConcurrent fans out span extraction across secs with a
// max-concurrency semaphore. Each extraction's outcome is written back
// into the matching slot's `span` field. Errors are logged and dropped
// — span extraction is best-effort.
func runSpansConcurrent(ctx context.Context, extractor *retrieval.SpanExtractor, query string, secs []sectionWithContent, maxConcurrency int, logger *slog.Logger) {
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

// handleAnswer runs retrieval + per-section answer-span extraction +
// a synthesis LLM call, returning a quote-grounded answer plus
// citations in a single round-trip. This is the most regulator-
// defensible thing the engine can produce — every citation carries a
// section ID, page range (when known), and the verbatim quote the
// answer relies on.
//
// Body: { document_id, query, model?, max_tokens?, reserved_for_prompt?,
// max_parallel_calls?, max_sections?, max_answer_tokens? }.
// Response: { document_id, query, answer, citations:
//
//	[{section_id, title, page_start, page_end, quote}], strategy,
//	model, usage, elapsed_ms }.
func (d Deps) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if d.LLM == nil {
		writeErr(w, http.StatusNotImplemented, "answer endpoint requires an LLM client")
		return
	}
	if d.Strategy == nil {
		writeErr(w, http.StatusServiceUnavailable, "no retrieval strategy configured")
		return
	}

	var body struct {
		DocumentID        tree.DocumentID `json:"document_id"`
		Query             string          `json:"query"`
		Model             string          `json:"model"`
		MaxTokens         int             `json:"max_tokens"`
		ReservedForPrompt int             `json:"reserved_for_prompt"`
		MaxParallelCalls  int             `json:"max_parallel_calls"`
		MaxSections       int             `json:"max_sections"`
		MaxAnswerTokens   int             `json:"max_answer_tokens"`
		// EnablePlanning opts this request into the Phase 2.1 query
		// planner. See handleQuery for the same field's semantics.
		EnablePlanning *bool `json:"enable_planning"`
		// EnableReRank opts this request into the Phase 2.3 re-rank
		// pass. Synthesis then sees the re-ranked top-k. Overrides
		// retrieval.rerank.enabled.
		EnableReRank *bool `json:"enable_rerank"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.DocumentID == "" || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_id and query are required")
		return
	}

	t, err := d.DB.LoadTree(r.Context(), body.DocumentID, standaloneOrgID, "")
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

	plan, planUsage := d.runPlanner(r.Context(), body.Query, body.EnablePlanning)
	totalUsage.Add(planUsage)

	ids, retrievalUsage, err := d.runSelectionWithUsage(r.Context(), t, plan, body.Query, budget)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "retrieval failed: "+err.Error())
		return
	}
	totalUsage.Add(retrievalUsage)

	maxSections := body.MaxSections
	if maxSections <= 0 {
		maxSections = d.Answer.MaxSections
	}
	if maxSections <= 0 {
		maxSections = 5
	}
	if len(ids) > maxSections {
		ids = ids[:maxSections]
	}

	// Load each section's content.
	enriched := make([]sectionWithContent, 0, len(ids))
	for _, id := range ids {
		sec := t.FindByID(id)
		if sec == nil {
			continue
		}
		var content string
		if sec.ContentRef != "" {
			rc, _, err := d.Storage.Get(r.Context(), sec.ContentRef)
			if err == nil {
				raw, _ := io.ReadAll(rc)
				rc.Close()
				content = string(raw)
			}
		}
		enriched = append(enriched, sectionWithContent{sec: sec, content: content})
	}

	// Optional: content-aware re-rank before synthesis sees the
	// evidence. When TopK is set the synthesis prompt only ever sees
	// the post-rerank top-k, keeping the answer focused on the
	// best-evidence sections.
	if d.reRankEnabled(body.EnableReRank) {
		var reRankUsage retrieval.Usage
		enriched, reRankUsage = d.runReRank(r.Context(), enriched, body.Query, body.Model)
		totalUsage.Add(reRankUsage)
	}

	// Always extract spans for /v1/answer — they ground each citation.
	spanExtractor := d.spanExtractor(body.Model)
	runSpansConcurrent(r.Context(), spanExtractor, body.Query, enriched, d.AnswerSpan.MaxConcurrency, d.Logger)

	// Synthesise. Feed only the spans (when available) + section
	// titles into the prompt so the model stays grounded in the
	// retrieved evidence.
	synthModel := d.Answer.Model
	if synthModel == "" {
		synthModel = body.Model
	}
	if synthModel == "" {
		synthModel = d.LLMModel
	}
	maxAnswerTokens := body.MaxAnswerTokens
	if maxAnswerTokens <= 0 {
		maxAnswerTokens = d.Answer.MaxAnswerTokens
	}
	if maxAnswerTokens <= 0 {
		maxAnswerTokens = 1024
	}

	answerText, synthUsage, err := synthesiseAnswer(r.Context(), d.LLM, synthModel, body.Query, plan, enriched, maxAnswerTokens)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "synthesis failed: "+err.Error())
		return
	}
	totalUsage.Add(synthUsage)

	citations := make([]map[string]any, 0, len(enriched))
	for _, e := range enriched {
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
		if e.hasScore {
			c["score"] = e.score
		}
		citations = append(citations, c)
	}

	resp := map[string]any{
		"document_id": body.DocumentID,
		"query":       body.Query,
		"answer":      answerText,
		"citations":   citations,
		"strategy":    d.Strategy.Name(),
		"model":       synthModel,
		"usage": map[string]any{
			"input_tokens":  totalUsage.InputTokens,
			"output_tokens": totalUsage.OutputTokens,
			"total_tokens":  totalUsage.TotalTokens,
			"cost_usd":      totalUsage.CostUSD,
			"llm_calls":     totalUsage.LLMCalls,
		},
		"elapsed_ms": time.Since(started).Milliseconds(),
	}
	if plan != nil {
		resp["plan"] = plan
	}
	writeJSON(w, http.StatusOK, resp)
}

// synthesiseAnswer runs one LLM call producing the final answer from
// retrieved sections + their extracted spans. The model is told to
// cite by section ID. When plan is non-nil its structured hints
// (intent, entities, expected_doc_areas, sub_questions) are folded
// into the prompt as a short "Planner notes" block so the model can
// reason with the same understanding the retrieval pipeline used.
func synthesiseAnswer(ctx context.Context, client llmgate.Client, model, query string, plan *retrieval.Plan, secs []sectionWithContent, maxAnswerTokens int) (string, retrieval.Usage, error) {
	var b strings.Builder
	b.WriteString("You are answering a user's question using ONLY the evidence below.\n\n")
	b.WriteString("User query:\n")
	b.WriteString(query)
	if plan != nil {
		writePlanHints(&b, plan)
	}
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

// handleQueryMulti accepts { document_ids, query, model?, max_tokens?,
// reserved_for_prompt?, max_parallel_calls?, max_sections? } and runs the
// retrieval strategy against every document in parallel, returning
// per-document results.
func (d Deps) handleQueryMulti(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DocumentIDs       []tree.DocumentID `json:"document_ids"`
		Query             string            `json:"query"`
		Model             string            `json:"model"`
		MaxTokens         int               `json:"max_tokens"`
		ReservedForPrompt int               `json:"reserved_for_prompt"`
		MaxParallelCalls  int               `json:"max_parallel_calls"`
		MaxSections       int               `json:"max_sections"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(body.DocumentIDs) == 0 || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_ids (non-empty) and query are required")
		return
	}
	if d.MultiDoc == nil {
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
	result, err := d.MultiDoc.Query(r.Context(), standaloneOrgID, "", body.DocumentIDs, body.Query, budget)
	if err != nil {
		d.Logger.Error("query/multi: failed", "err", err)
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
				rc, _, err := d.Storage.Get(r.Context(), sec.ContentRef)
				if err == nil {
					raw, _ := io.ReadAll(rc)
					rc.Close()
					content = string(raw)
				}
			}
			s := map[string]any{
				"id":          sec.ID,
				"parent_id":   sec.ParentID,
				"title":       sec.Title,
				"summary":     sec.Summary,
				"token_count": sec.TokenCount,
				"content":     content,
			}
			if sec.PageStart > 0 {
				s["page_start"] = sec.PageStart
			}
			if sec.PageEnd > 0 {
				s["page_end"] = sec.PageEnd
			}
			if len(sec.CandidateQuestions) > 0 {
				s["candidate_questions"] = sec.CandidateQuestions
			}
			sections = append(sections, s)
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
		"strategy":   d.Strategy.Name(),
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

// --- internal queue webhook ---

// handleQueueWebhook is the endpoint QStash POSTs to. It verifies the
// Upstash-Signature header, then dispatches the decoded payload into the
// queue handler registered for {kind}.
//
// Only wired up when the configured queue is *queue.QStash; with other
// drivers (River, Asynq) the route is present but returns 404-ish: there
// is no webhook consumer to run.
func (d Deps) handleQueueWebhook(w http.ResponseWriter, r *http.Request) {
	qq, ok := d.Queue.(*queue.QStash)
	if !ok {
		writeErr(w, http.StatusNotFound, "webhook not enabled: queue driver is not qstash")
		return
	}

	kind := queue.JobKind(chi.URLParam(r, "kind"))
	if kind == "" {
		writeErr(w, http.StatusBadRequest, "missing kind")
		return
	}

	// Read the full body up front — verification hashes it, and the
	// handler needs the raw bytes too.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	_ = r.Body.Close()

	// Signature check. If no verifier is configured we refuse to proceed:
	// an unauthenticated webhook endpoint that executes jobs is an open
	// door to the worker. Local-only dev can set VLE_QSTASH_CURRENT_SIGNING_KEY
	// to any string and sign test requests with it.
	v := qq.Verifier()
	if v == nil {
		writeErr(w, http.StatusUnauthorized, "qstash signing key not configured")
		return
	}

	// Reconstruct the URL QStash signed against. We prefer the configured
	// WebhookBaseURL over r.Host — behind TLS terminators r.TLS and
	// r.Host are unreliable, and the operator already told us the
	// canonical external URL at boot.
	expectedURL := strings.TrimRight(qq.WebhookBaseURL(), "/") + r.URL.Path

	sig := r.Header.Get("Upstash-Signature")
	if err := v.Verify(sig, body, expectedURL); err != nil {
		if d.Logger != nil {
			d.Logger.Warn("qstash verify failed", "err", err, "kind", kind)
		}
		writeErr(w, http.StatusUnauthorized, "invalid qstash signature")
		return
	}

	// Body shape: either a full queue.Job (has `kind` + `payload`) or the
	// bare payload (`kind` is already in the URL). Accept both — the
	// dashboard publishes bare payloads today; the engine's own Enqueue
	// publishes full Jobs.
	payload := body
	var maybe struct {
		Kind    queue.JobKind   `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &maybe); err == nil && maybe.Kind != "" && len(maybe.Payload) > 0 {
		if maybe.Kind != kind {
			writeErr(w, http.StatusBadRequest, "kind in body does not match URL")
			return
		}
		payload = maybe.Payload
	}

	if err := qq.Dispatch(r.Context(), kind, payload); err != nil {
		if errors.Is(err, queue.ErrUnknownKind) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if d.Logger != nil {
			d.Logger.Error("qstash dispatch failed", "err", err, "kind", kind)
		}
		// 5xx → QStash will retry per the publish-time Upstash-Retries header.
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- planning helpers ---

// planningEnabled reports whether the request should go through the
// planner. The per-request body field (when present) wins over the
// server-side config; a nil body field falls back to the config.
func (d Deps) planningEnabled(bodyOverride *bool) bool {
	if d.Planner == nil {
		return false
	}
	if bodyOverride != nil {
		return *bodyOverride
	}
	return d.Planning.Enabled
}

// runPlanner issues the planning LLM call when planning is enabled.
// Returns (nil, zero usage) when planning is off, the query is empty,
// the planner is missing, or the planner gracefully degraded to a
// no-plan result. Transport errors from the planner are LOGGED but not
// propagated — the engine continues with the original query rather
// than 500ing a working retrieval request because of a planner blip.
func (d Deps) runPlanner(ctx context.Context, query string, bodyOverride *bool) (*retrieval.Plan, retrieval.Usage) {
	if !d.planningEnabled(bodyOverride) {
		return nil, retrieval.Usage{}
	}
	plan, usage, err := d.Planner.Plan(ctx, query)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn("planner: failed; continuing without plan", "err", err)
		}
		return nil, usage
	}
	return plan, usage
}

// runSelection picks section IDs for the query, optionally going
// through the Decomposer when the plan is multi-hop AND planning-level
// decomposition is enabled. Returns the same []SectionID Strategy.Select
// would.
func (d Deps) runSelection(ctx context.Context, t *tree.Tree, plan *retrieval.Plan, query string, budget retrieval.ContextBudget) ([]tree.SectionID, error) {
	if d.shouldDecompose(plan) {
		ids, _, err := retrieval.NewDecomposer(d.Strategy).DecomposedSelect(ctx, t, plan, query, budget)
		return ids, err
	}
	return d.Strategy.Select(ctx, t, query, budget)
}

// runSelectionWithUsage is the cost-tracking variant used by /v1/answer.
// Returns the selected IDs plus the Usage accumulated during selection
// (across all sub-questions for multi-hop plans).
func (d Deps) runSelectionWithUsage(ctx context.Context, t *tree.Tree, plan *retrieval.Plan, query string, budget retrieval.ContextBudget) ([]tree.SectionID, retrieval.Usage, error) {
	if d.shouldDecompose(plan) {
		return retrieval.NewDecomposer(d.Strategy).DecomposedSelect(ctx, t, plan, query, budget)
	}
	if cs, ok := d.Strategy.(retrieval.CostStrategy); ok {
		res, err := cs.SelectWithCost(ctx, t, query, budget)
		if err != nil {
			return nil, retrieval.Usage{}, err
		}
		if res == nil {
			return nil, retrieval.Usage{}, nil
		}
		return res.SelectedIDs, res.Usage, nil
	}
	ids, err := d.Strategy.Select(ctx, t, query, budget)
	if err != nil {
		return nil, retrieval.Usage{}, err
	}
	return ids, retrieval.Usage{}, nil
}

// shouldDecompose returns true when the plan is multi-hop AND
// decomposition is enabled at the config level. The Decomposer
// itself short-circuits to Strategy.Select when the plan is missing
// or non-multi-hop, but we duplicate that check here so we avoid
// allocating a Decomposer when it would be a no-op.
func (d Deps) shouldDecompose(plan *retrieval.Plan) bool {
	if plan == nil || !plan.IsMultiHop || len(plan.SubQuestions) == 0 {
		return false
	}
	return d.Planning.Decompose
}

// --- re-rank helpers ---

// reRankEnabled reports whether the request should go through the
// re-rank pass. The per-request body field (when present) wins over
// the server-side config; a nil body field falls back to the config.
//
// Returns false when no LLM client is wired or when no ReRanker is
// configured, regardless of intent — re-rank without an LLM is
// physically impossible.
func (d Deps) reRankEnabled(bodyOverride *bool) bool {
	if d.ReRanker == nil || d.LLM == nil {
		return false
	}
	if bodyOverride != nil {
		return *bodyOverride
	}
	return d.ReRank.Enabled
}

// runReRank executes the re-rank pass over the loaded section slice
// and returns the reordered slice plus the LLM Usage spent. On any
// failure the original slice is returned (with the same hasScore
// values it had on input — i.e. unchanged) so the caller never has
// to think about partial state. The error is LOGGED, not returned —
// re-rank is best-effort and a failure must never abort the request.
//
// requestModel is the model the request asked for. When the
// ReRanker has its own Model set (the config-level override), that
// wins; the request model is the fall-through.
func (d Deps) runReRank(ctx context.Context, enriched []sectionWithContent, query, requestModel string) ([]sectionWithContent, retrieval.Usage) {
	if d.ReRanker == nil || d.LLM == nil || len(enriched) == 0 {
		return enriched, retrieval.Usage{}
	}

	// Apply the model fall-through: config override → request model →
	// engine default. We don't mutate d.ReRanker since Deps is shared
	// across requests; instead build a shallow copy with the chosen
	// model. This is the same pattern spanExtractor() uses.
	ranker := *d.ReRanker
	if ranker.Model == "" {
		if requestModel != "" {
			ranker.Model = requestModel
		} else {
			ranker.Model = d.LLMModel
		}
	}

	candidates := make([]retrieval.SectionContent, len(enriched))
	for i, e := range enriched {
		candidates[i] = retrieval.SectionContent{
			ID:      e.sec.ID,
			Title:   e.sec.Title,
			Content: e.content,
		}
	}

	scored, usage, err := ranker.ReRank(ctx, query, candidates)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn("rerank: failed; preserving strategy order", "err", err)
		}
		// ReRank returns input order on error so we *could* apply it
		// (it'd just stamp score=0 on everything). Skip — the caller
		// shouldn't see score=0 on every section when re-rank
		// physically failed.
		return enriched, usage
	}
	if len(scored) == 0 {
		return enriched, usage
	}

	reordered := reorderByScore(enriched, scored)
	if d.ReRank.TopK > 0 && len(reordered) > d.ReRank.TopK {
		reordered = reordered[:d.ReRank.TopK]
	}
	return reordered, usage
}

// reorderByScore takes the loaded section slice and the model's
// scored output (already sorted descending by score by the
// ReRanker), and returns a new slice in the same order as scored
// with each entry carrying the per-section score.
//
// Defensive: every input enriched section appears in the output
// exactly once, in the order dictated by scored. If scored is
// missing an input ID (shouldn't happen — ReRank's contract is to
// surface every input ID), that section is appended at the end with
// hasScore=false so the response stays complete.
func reorderByScore(enriched []sectionWithContent, scored []retrieval.ScoredSection) []sectionWithContent {
	byID := make(map[tree.SectionID]int, len(enriched))
	for i, e := range enriched {
		byID[e.sec.ID] = i
	}

	out := make([]sectionWithContent, 0, len(enriched))
	taken := make([]bool, len(enriched))
	for _, s := range scored {
		idx, ok := byID[s.ID]
		if !ok || taken[idx] {
			continue
		}
		taken[idx] = true
		e := enriched[idx]
		e.hasScore = true
		e.score = s.Score
		out = append(out, e)
	}
	// Append anything ReRank didn't surface — invariant says this
	// should be empty, but a defence-in-depth check costs nothing.
	for i, e := range enriched {
		if !taken[i] {
			out = append(out, e)
		}
	}
	return out
}

// writePlanHints appends a short, model-readable "Planner notes" block
// describing the structured plan. Synthesis uses this to orient itself
// before reading the evidence.
func writePlanHints(b *strings.Builder, plan *retrieval.Plan) {
	if plan == nil {
		return
	}
	b.WriteString("\n\nPlanner notes (structured understanding of the query):")
	if plan.Intent != "" {
		fmt.Fprintf(b, "\n- intent: %s", plan.Intent)
	}
	if len(plan.Entities) > 0 {
		fmt.Fprintf(b, "\n- entities: %s", strings.Join(plan.Entities, ", "))
	}
	if len(plan.ExpectedDocAreas) > 0 {
		fmt.Fprintf(b, "\n- expected document areas: %s", strings.Join(plan.ExpectedDocAreas, ", "))
	}
	if plan.IsMultiHop && len(plan.SubQuestions) > 0 {
		b.WriteString("\n- sub-questions:")
		for _, q := range plan.SubQuestions {
			fmt.Fprintf(b, "\n  - %s", q)
		}
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func guessContentType(filename string) string {
	name := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(name, ".md"), strings.HasSuffix(name, ".markdown"):
		return "text/markdown"
	case strings.HasSuffix(name, ".txt"):
		return "text/plain"
	case strings.HasSuffix(name, ".html"), strings.HasSuffix(name, ".htm"):
		return "text/html"
	case strings.HasSuffix(name, ".pdf"):
		return "application/pdf"
	}
	return "application/octet-stream"
}
