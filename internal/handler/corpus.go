package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// corpusCatalog is the slice of *db.Pool corpus mode needs: list a
// scope's documents, or fetch named ones.
type corpusCatalog interface {
	ListDocuments(ctx context.Context, o db.ListDocumentsOpts) ([]db.Document, time.Time, error)
	GetDocument(ctx context.Context, id tree.DocumentID, orgID, storeID string) (*db.Document, error)
}

// CorpusHandler serves corpus mode (HAL-3075): one question over a whole
// collection, routed, read and merged on the Judge, with bridge hops for
// multi-hop questions. /v1/query/corpus returns the merged evidence;
// /v1/answer/corpus also writes one answer over it.
type CorpusHandler struct {
	logger    *slog.Logger
	catalog   corpusCatalog
	library   *retrieval.CorpusLibrary
	navigator retrieval.CorpusNavigator
	// answers writes the answer and places its citations; nil makes
	// /v1/answer/corpus answer 501.
	answers *AnswerStoreHandler
}

// NewCorpusHandler builds the handler. A nil library (no Judge
// configured) makes both routes answer 501.
func NewCorpusHandler(logger *slog.Logger, catalog corpusCatalog, library *retrieval.CorpusLibrary, navigator retrieval.CorpusNavigator, answers *AnswerStoreHandler) *CorpusHandler {
	return &CorpusHandler{logger: logger, catalog: catalog, library: library, navigator: navigator, answers: answers}
}

type corpusRequest struct {
	Query string `json:"query"`
	// DocumentIDs limits the collection; empty means every ready document
	// in the caller's org and store.
	DocumentIDs         []tree.DocumentID `json:"document_ids"`
	MaxDocuments        int               `json:"max_documents"`
	MaxPagesPerDocument int               `json:"max_pages_per_document"`
	MaxEvidence         int               `json:"max_evidence"`
	MaxHops             int               `json:"max_hops"`
	// Answer-only fields.
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
}

const (
	corpusMaxDocumentsCap = 32
	corpusMaxHopsCap      = 5
	corpusListPage        = 200
	// corpusDeclineBelow is the coverage under which /v1/answer/corpus
	// tells the generator the collection may not answer the question.
	corpusDeclineBelow = 0.25
)

func (h *CorpusHandler) decode(w http.ResponseWriter, r *http.Request) (string, corpusRequest, bool) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return "", corpusRequest{}, false
	}
	var body corpusRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return "", body, false
	}
	body.Query = strings.TrimSpace(body.Query)
	if body.Query == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return "", body, false
	}
	if h.library == nil || h.navigator.Judge == nil {
		writeErr(w, http.StatusNotImplemented, "corpus mode needs a Judge (llm.judge) configured")
		return "", body, false
	}
	return orgID, body, true
}

// run resolves the collection, builds its documents and navigates.
func (h *CorpusHandler) run(ctx context.Context, orgID, store string, body corpusRequest) (*retrieval.CorpusResult, error) {
	refs, err := h.refs(ctx, orgID, store, body.DocumentIDs)
	if err != nil {
		return nil, err
	}
	docs, buildErrs := h.library.Docs(ctx, orgID, store, refs)
	nav := h.navigator
	if body.MaxDocuments > 0 {
		nav.MaxDocuments = min(body.MaxDocuments, corpusMaxDocumentsCap)
	}
	if body.MaxPagesPerDocument > 0 {
		nav.MaxPagesPerDocument = body.MaxPagesPerDocument
	}
	if body.MaxEvidence > 0 {
		nav.MaxEvidence = body.MaxEvidence
	}
	if body.MaxHops > 0 {
		nav.MaxHops = min(body.MaxHops, corpusMaxHopsCap)
	}
	res, err := nav.Navigate(ctx, body.Query, docs)
	if err != nil {
		return nil, err
	}
	res.Considered = len(refs)
	for id, e := range buildErrs {
		if res.Errors == nil {
			res.Errors = map[tree.DocumentID]string{}
		}
		res.Errors[id] = e.Error()
	}
	return res, nil
}

// refs lists the collection: the named documents, or every ready one in
// scope. Only ready documents are read.
func (h *CorpusHandler) refs(ctx context.Context, orgID, store string, ids []tree.DocumentID) ([]retrieval.CorpusRef, error) {
	toRef := func(d db.Document) retrieval.CorpusRef {
		return retrieval.CorpusRef{ID: d.ID, Title: d.Title, UpdatedAt: d.UpdatedAt, TOC: d.TOCTree}
	}
	if len(ids) > 0 {
		out := make([]retrieval.CorpusRef, len(ids))
		keep := make([]bool, len(ids))
		var mu sync.Mutex
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(16)
		for i, id := range ids {
			g.Go(func() error {
				d, err := h.catalog.GetDocument(gctx, id, orgID, store)
				if err != nil || d == nil || d.Status != db.StatusReady {
					return nil // not found in scope, or not ready: not part of the collection
				}
				mu.Lock()
				out[i], keep[i] = toRef(*d), true
				mu.Unlock()
				return nil
			})
		}
		_ = g.Wait()
		refs := make([]retrieval.CorpusRef, 0, len(ids))
		for i := range out {
			if keep[i] {
				refs = append(refs, out[i])
			}
		}
		return refs, nil
	}
	var (
		refs   []retrieval.CorpusRef
		cursor time.Time
	)
	for {
		page, next, err := h.catalog.ListDocuments(ctx, db.ListDocumentsOpts{
			OrgID: orgID, StoreID: store, Limit: corpusListPage, Cursor: cursor, Status: db.StatusReady,
		})
		if err != nil {
			return nil, err
		}
		for _, d := range page {
			refs = append(refs, toRef(d))
		}
		if next.IsZero() || len(page) == 0 {
			break
		}
		cursor = next
	}
	return refs, nil
}

func corpusResponse(query string, res *retrieval.CorpusResult, started time.Time) map[string]any {
	docs := res.Documents
	if len(docs) > 25 {
		docs = docs[:25] // the full ranking of a large collection is noise past the top
	}
	return map[string]any{
		"query":                query,
		"strategy":             "corpus",
		"evidence":             res.Evidence,
		"documents":            docs,
		"documents_considered": res.Considered,
		"hops":                 res.Hops,
		"coverage":             res.Coverage,
		"errors":               res.Errors,
		"judge_requests":       res.Requests,
		"usage":                usageMap(res.Usage),
		"elapsed_ms":           time.Since(started).Milliseconds(),
	}
}

// HandleQueryCorpus is POST /v1/query/corpus.
func (h *CorpusHandler) HandleQueryCorpus(w http.ResponseWriter, r *http.Request) {
	orgID, body, ok := h.decode(w, r)
	if !ok {
		return
	}
	started := time.Now()
	res, err := h.run(r.Context(), orgID, storeID(r), body)
	if err != nil {
		h.logger.Error("query/corpus: failed", "err", err)
		writeErr(w, http.StatusBadGateway, "corpus retrieval failed: "+err.Error())
		return
	}
	resp := corpusResponse(body.Query, res, started)
	withTiming(r, resp)
	writeJSON(w, http.StatusOK, resp)
}

// HandleAnswerCorpus is POST /v1/answer/corpus: the evidence of
// /v1/query/corpus, then one generated answer citing document and page,
// each citation placed on its page.
func (h *CorpusHandler) HandleAnswerCorpus(w http.ResponseWriter, r *http.Request) {
	orgID, body, ok := h.decode(w, r)
	if !ok {
		return
	}
	if h.answers == nil || h.answers.llm == nil {
		writeErr(w, http.StatusNotImplemented, "corpus answers require an LLM-backed configuration")
		return
	}
	started := time.Now()
	store := storeID(r)
	res, err := h.run(r.Context(), orgID, store, body)
	if err != nil {
		h.logger.Error("answer/corpus: retrieval failed", "err", err)
		writeErr(w, http.StatusBadGateway, "corpus retrieval failed: "+err.Error())
		return
	}
	resp := corpusResponse(body.Query, res, started)
	if len(res.Evidence) == 0 {
		resp["answer"] = "No document in this collection appears to hold what the question needs."
		resp["citations"] = []any{}
		resp["elapsed_ms"] = time.Since(started).Milliseconds()
		withTiming(r, resp)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	sections := make([]relevantSection, 0, len(res.Evidence))
	for i, e := range res.Evidence {
		sections = append(sections, relevantSection{
			index: i + 1, docID: e.DocumentID, docTitle: e.DocumentTitle, sectionTitle: e.SectionTitle,
			startPage: e.Page, endPage: e.Page, content: e.Text,
		})
	}
	query := body.Query
	if res.Coverage < corpusDeclineBelow {
		// The Judge found the evidence incomplete: tell the generator, so a
		// question the collection does not answer is declined rather than
		// completed from the model's own knowledge.
		query += "\n\n(Retrieval judged these sections unlikely to contain everything the question needs. " +
			"If they do not answer it, say so and state what is missing.)"
	}
	model := body.Model
	if model == "" {
		model = h.answers.defaultModel()
	}
	answer, cited, synthUsage, err := h.answers.generate(r.Context(), model, query, sections, body.MaxTokens)
	if err != nil {
		h.logger.Error("answer/corpus: generate failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "answer generation failed: "+err.Error())
		return
	}
	total := res.Usage
	total.Add(synthUsage)
	citations := buildStoreCitations(sections, cited)
	h.answers.annotate(r.Context(), orgID, store, citations)
	resp["answer"] = answer
	resp["citations"] = citations
	resp["usage"] = usageMap(total)
	resp["retrieval_usage"] = usageMap(res.Usage)
	resp["elapsed_ms"] = time.Since(started).Milliseconds()
	setTokenHeaders(w.Header(), resp)
	withTiming(r, resp)
	writeJSON(w, http.StatusOK, resp)
}
