package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hallelx2/llmgate"
	"golang.org/x/sync/errgroup"

	enginecfg "github.com/hallelx2/vectorless-engine/pkg/config"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// storeTreeLoader is the narrow slice of *db.Pool the store handler needs
// to materialise each document's section tree. *db.Pool satisfies it.
type storeTreeLoader interface {
	LoadTree(ctx context.Context, docID tree.DocumentID, orgID, storeID string) (*tree.Tree, error)
}

// AnswerStoreHandler answers a question across a SET of documents (a
// "store" / collection) and synthesises a SINGLE grounded answer with
// per-document citations. It is reasoning-first, not chunk-retrieval:
//
//	map    — the FULL agentic tree-walk runs on EACH document: it reads
//	         the document's structure and navigates it hop by hop to a
//	         grounded per-document answer + page citations. No chunking,
//	         no embeddings — every document is reasoned over exactly like
//	         the single-document /v1/answer/treewalk.
//	reduce — one LLM call reads the per-document answers (each labelled
//	         with its title) and writes one coherent answer, citing the
//	         documents it drew on with [n] markers.
//
// max_depth controls the per-document tree-walk hop budget (0 = the
// engine's full/default depth). max_docs bounds how many documents are
// tree-walked (0 = all supplied) — a cost valve for large collections.
type AnswerStoreHandler struct {
	logger          *slog.Logger
	db              storeTreeLoader
	treewalk        *retrieval.TreeWalkStrategy
	treeWalkEnabled bool
	llm             llmgate.Client
	llmModel        string
}

// NewAnswerStoreHandler creates an AnswerStoreHandler. It returns 501 at
// request time when the tree-walk strategy or LLM is not configured.
func NewAnswerStoreHandler(
	logger *slog.Logger,
	db storeTreeLoader,
	treewalk *retrieval.TreeWalkStrategy,
	treeWalkCfg enginecfg.TreeWalkBlock,
	llm llmgate.Client,
	llmModel string,
) *AnswerStoreHandler {
	return &AnswerStoreHandler{
		logger:          logger,
		db:              db,
		treewalk:        treewalk,
		treeWalkEnabled: treeWalkCfg.Enabled,
		llm:             llm,
		llmModel:        llmModel,
	}
}

type answerStoreRequest struct {
	DocumentIDs []tree.DocumentID `json:"document_ids"`
	Query       string            `json:"query"`
	Model       string            `json:"model"`
	MaxDepth    int               `json:"max_depth"` // per-doc tree-walk hop budget; 0 = full
	MaxDocs     int               `json:"max_docs"`  // cap documents tree-walked; 0 = all
	MaxTokens   int               `json:"max_tokens"`
}

// docAnswer is one document's tree-walk result in the map stage.
type docAnswer struct {
	index      int // 1-based citation index (assigned to contributing docs)
	docID      tree.DocumentID
	docTitle   string
	answer     string
	confidence float64
	startPage  int
	endPage    int
	sectionIDs []tree.SectionID
}

const (
	storeAnswerMapConcurrency = 6
	storeAnswerQuoteLen       = 240
)

// HandleAnswerStore is POST /v1/answer/store.
func (h *AnswerStoreHandler) HandleAnswerStore(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrgID(w, r)
	if !ok {
		return
	}
	var body answerStoreRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	body.Query = strings.TrimSpace(body.Query)
	if len(body.DocumentIDs) == 0 || body.Query == "" {
		writeErr(w, http.StatusBadRequest, "document_ids (non-empty) and query are required")
		return
	}
	if h.llm == nil || h.treewalk == nil || !h.treeWalkEnabled {
		writeErr(w, http.StatusNotImplemented, "store answers require the tree-walk strategy + an LLM (retrieval.treewalk.enabled=true)")
		return
	}

	docIDs := body.DocumentIDs
	if body.MaxDocs > 0 && len(docIDs) > body.MaxDocs {
		docIDs = docIDs[:body.MaxDocs]
	}
	model := body.Model
	if model == "" {
		model = h.llmModel
	}

	started := time.Now()

	// ── MAP: full tree-walk on every document, in parallel ─────────
	perDoc, mapUsage := h.mapTreeWalk(r.Context(), orgID, storeID(r), docIDs, body.Query, model, body.MaxDepth)

	// Keep only documents that produced a real, non-refusal answer.
	contributing := make([]docAnswer, 0, len(perDoc))
	for _, d := range perDoc {
		if isNonAnswer(d.answer) {
			continue
		}
		d.index = len(contributing) + 1
		contributing = append(contributing, d)
	}

	totalUsage := mapUsage
	if len(contributing) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"query":                  body.Query,
			"answer":                 "The documents in this collection do not appear to address this query.",
			"citations":              []any{},
			"documents_searched":     len(docIDs),
			"documents_with_matches": 0,
			"confidence":             0.0,
			"usage":                  usageMap(totalUsage),
			"elapsed_ms":             time.Since(started).Milliseconds(),
		})
		return
	}

	// ── REDUCE: synthesise the per-document answers ────────────────
	answer, citedIdx, synthUsage, err := h.synthesise(r.Context(), model, body.Query, contributing, body.MaxTokens)
	if err != nil {
		h.logger.Error("answer/store: reduce failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "answer synthesis failed: "+err.Error())
		return
	}
	totalUsage.Add(synthUsage)

	writeJSON(w, http.StatusOK, map[string]any{
		"query":                  body.Query,
		"answer":                 answer,
		"citations":              buildStoreCitations(contributing, citedIdx),
		"documents_searched":     len(docIDs),
		"documents_with_matches": len(contributing),
		"usage":                  usageMap(totalUsage),
		"elapsed_ms":             time.Since(started).Milliseconds(),
	})
}

// mapTreeWalk runs the full tree-walk on each document concurrently and
// returns each document's grounded answer + citations, plus the summed
// usage. Per-document failures are logged and skipped (partial results
// are fine). maxDepth overrides the per-document hop budget (0 = default).
func (h *AnswerStoreHandler) mapTreeWalk(ctx context.Context, orgID, storeID string, docIDs []tree.DocumentID, query, model string, maxDepth int) ([]docAnswer, retrieval.Usage) {
	var (
		mu    sync.Mutex
		out   = make([]docAnswer, 0, len(docIDs))
		usage retrieval.Usage
	)
	budget := retrieval.ContextBudget{ModelName: model, MaxTokens: 100000, ReservedForPrompt: 4000}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(storeAnswerMapConcurrency)
	for _, docID := range docIDs {
		g.Go(func() error {
			t, err := h.db.LoadTree(gctx, docID, orgID, storeID)
			if err != nil || t == nil {
				h.logger.Warn("answer/store: load tree", "doc", docID, "err", err)
				return nil
			}
			// Per-request copy so a hop override never mutates the shared
			// strategy other goroutines are reading.
			perReq := *h.treewalk
			perReq.OnEvent = nil // non-streaming
			if maxDepth > 0 {
				perReq.MaxHops = maxDepth
			}
			res, err := perReq.SelectWithCost(gctx, t, query, budget)
			if err != nil {
				h.logger.Warn("answer/store: treewalk", "doc", docID, "err", err)
				return nil
			}

			// Resolve cited page ranges → section ids + a page span.
			var (
				secIDs           []tree.SectionID
				minStart, maxEnd int
				seen             = map[tree.SectionID]struct{}{}
			)
			for _, pr := range res.CitedPages {
				if minStart == 0 || pr[0] < minStart {
					minStart = pr[0]
				}
				if pr[1] > maxEnd {
					maxEnd = pr[1]
				}
				for _, id := range retrieval.SectionIDsOverlapping(t, pr[0], pr[1]) {
					if _, dup := seen[id]; !dup {
						seen[id] = struct{}{}
						secIDs = append(secIDs, id)
					}
				}
			}

			mu.Lock()
			out = append(out, docAnswer{
				docID:      docID,
				docTitle:   t.Title,
				answer:     strings.TrimSpace(res.Reasoning),
				confidence: res.Confidence,
				startPage:  minStart,
				endPage:    maxEnd,
				sectionIDs: secIDs,
			})
			usage.Add(res.Usage)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	// Deterministic order: highest confidence first, then title.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].confidence != out[j].confidence {
			return out[i].confidence > out[j].confidence
		}
		return out[i].docTitle < out[j].docTitle
	})
	return out, usage
}

// synthesise runs the single reducer LLM call over the per-document
// answers. Returns the final answer (with [n] markers), the cited doc
// indices, and usage.
func (h *AnswerStoreHandler) synthesise(ctx context.Context, model, query string, docs []docAnswer, maxTokens int) (string, []int, retrieval.Usage, error) {
	var ev strings.Builder
	for _, d := range docs {
		title := d.docTitle
		if title == "" {
			title = string(d.docID)
		}
		fmt.Fprintf(&ev, "[%d] %s", d.index, title)
		if p := formatPageRange(d.startPage, d.endPage); p != "" {
			fmt.Fprintf(&ev, " (%s)", p)
		}
		ev.WriteString(":\n")
		ev.WriteString(d.answer)
		ev.WriteString("\n\n")
	}

	if maxTokens <= 0 {
		maxTokens = 1024
	}
	req := llmgate.Request{
		Model:       model,
		MaxTokens:   maxTokens,
		Temperature: 0,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: storeAnswerSystemPrompt},
			{Role: llmgate.RoleUser, Content: fmt.Sprintf("QUESTION:\n%s\n\nPER-DOCUMENT ANSWERS:\n%s\nReply with ONLY the JSON object.", query, ev.String())},
		},
	}
	resp, err := h.llm.Complete(ctx, req)
	if err != nil {
		return "", nil, retrieval.Usage{}, err
	}
	usage := retrieval.Usage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		CostUSD:      resp.Usage.CostUSD,
		LLMCalls:     1,
	}
	answer, cited := parseStoreAnswer(resp.Content, len(docs))
	return answer, cited, usage, nil
}

const storeAnswerSystemPrompt = `You are given a QUESTION and a set of ANSWERS, each produced by carefully reading ONE document in a collection. Each is prefixed with a number in brackets, e.g. [1], [2], and its source title.

Your job is to write ONE coherent answer to the question by synthesising across these per-document answers.

RULES:
- Use ONLY the supplied per-document answers. Do not invent facts.
- Cite the document(s) you draw on inline with their bracketed number, e.g. "Consent must be freely given [1]." Cite every claim.
- Synthesise: when several documents contribute, combine them into one answer and note agreements or differences. When only some are relevant, use only those.
- If none of the answers actually address the question, say so plainly.
- Be concise and well-structured.

Reply with EXACTLY one JSON object, no markdown fences:
{"answer":"<answer text with [n] citation markers>","cited":[<the document numbers you cited>]}`

// storeAnswerJSON is the reducer's expected output shape.
type storeAnswerJSON struct {
	Answer string `json:"answer"`
	Cited  []int  `json:"cited"`
}

// parseStoreAnswer extracts the answer + cited indices from the model's
// reply, tolerant of code fences / surrounding prose. Falls back to the
// raw text (and citation markers scraped from it) when the JSON can't be
// parsed, so a formatting slip never drops the answer.
func parseStoreAnswer(raw string, maxIdx int) (string, []int) {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			var parsed storeAnswerJSON
			if err := json.Unmarshal([]byte(s[i:j+1]), &parsed); err == nil && strings.TrimSpace(parsed.Answer) != "" {
				return strings.TrimSpace(parsed.Answer), dedupeInRange(parsed.Cited, maxIdx)
			}
		}
	}
	return s, scrapeMarkers(s, maxIdx)
}

func dedupeInRange(idx []int, maxIdx int) []int {
	seen := make(map[int]struct{}, len(idx))
	out := make([]int, 0, len(idx))
	for _, n := range idx {
		if n < 1 || n > maxIdx {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// scrapeMarkers pulls [n] indices out of answer prose (fallback path).
func scrapeMarkers(s string, maxIdx int) []int {
	var nums []int
	for i := 0; i < len(s); i++ {
		if s[i] != '[' {
			continue
		}
		j := i + 1
		n := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			n = n*10 + int(s[j]-'0')
			j++
		}
		if j < len(s) && s[j] == ']' && n >= 1 {
			nums = append(nums, n)
		}
	}
	return dedupeInRange(nums, maxIdx)
}

// buildStoreCitations turns the cited document indices into the response
// citations array — one citation per contributing document, carrying its
// id, title, page span, section ids, and a short quote from its answer.
func buildStoreCitations(docs []docAnswer, cited []int) []map[string]any {
	byIdx := make(map[int]docAnswer, len(docs))
	for _, d := range docs {
		byIdx[d.index] = d
	}
	if len(cited) == 0 { // model cited nothing parseable → show all sources
		for _, d := range docs {
			cited = append(cited, d.index)
		}
	}
	out := make([]map[string]any, 0, len(cited))
	for _, n := range cited {
		d, ok := byIdx[n]
		if !ok {
			continue
		}
		c := map[string]any{
			"index":       d.index,
			"document_id": d.docID,
			"section_ids": d.sectionIDs,
			"quote":       snippet(d.answer, storeAnswerQuoteLen),
			"confidence":  d.confidence,
		}
		if d.docTitle != "" {
			c["document_title"] = d.docTitle
		}
		if d.startPage > 0 {
			c["start_page"] = d.startPage
			c["end_page"] = d.endPage
		}
		out = append(out, c)
	}
	return out
}

// isNonAnswer reports whether a per-document tree-walk answer is a refusal
// or empty, so it can be dropped from the synthesis.
func isNonAnswer(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) < 3 {
		return true
	}
	return strings.Contains(s, "does not address") ||
		strings.Contains(s, "not address this query") ||
		strings.Contains(s, "no relevant information") ||
		strings.Contains(s, "does not contain")
}

func snippet(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n], " ")
	if cut < n/2 {
		cut = n
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func formatPageRange(start, end int) string {
	switch {
	case start <= 0:
		return ""
	case end <= 0 || end == start:
		return fmt.Sprintf("p.%d", start)
	default:
		return fmt.Sprintf("pp.%d-%d", start, end)
	}
}

func usageMap(u retrieval.Usage) map[string]any {
	return map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"total_tokens":  u.TotalTokens,
		"cost_usd":      u.CostUSD,
		"llm_calls":     u.LLMCalls,
	}
}
