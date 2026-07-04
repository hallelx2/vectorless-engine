package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// AnswerStoreHandler answers a question across a SET of documents (a
// "store" / collection) and synthesises a SINGLE grounded answer with
// per-document citations. It is the map-reduce companion to the
// single-document tree-walk:
//
//	map    — retrieval.MultiDoc fans the strategy across every document
//	         and returns the relevant sections per document.
//	reduce — one LLM call reads the gathered, source-labelled evidence
//	         and writes one answer, citing sources with [n] markers.
//
// The caller passes the document_ids that make up the store (the
// dashboard resolves store → its ready documents); the org + store scope
// come from the injected headers, so a key can only ever read its own
// documents.
type AnswerStoreHandler struct {
	logger   *slog.Logger
	storage  storage.Storage
	multiDoc *retrieval.MultiDoc
	llm      llmgate.Client
	llmModel string
}

// NewAnswerStoreHandler creates an AnswerStoreHandler. llm/multiDoc may be
// nil in configurations without an LLM; the handler then returns 501.
func NewAnswerStoreHandler(
	logger *slog.Logger,
	store storage.Storage,
	multiDoc *retrieval.MultiDoc,
	llm llmgate.Client,
	llmModel string,
) *AnswerStoreHandler {
	return &AnswerStoreHandler{
		logger:   logger,
		storage:  store,
		multiDoc: multiDoc,
		llm:      llm,
		llmModel: llmModel,
	}
}

type answerStoreRequest struct {
	DocumentIDs       []tree.DocumentID `json:"document_ids"`
	Query             string            `json:"query"`
	Model             string            `json:"model"`
	MaxSectionsPerDoc int               `json:"max_sections_per_doc"`
	MaxTokens         int               `json:"max_tokens"`
}

// evidenceBlock is one source-labelled snippet handed to the reducer.
type evidenceBlock struct {
	index      int // 1-based global citation index
	docID      tree.DocumentID
	docTitle   string
	startPage  int
	endPage    int
	sectionIDs []tree.SectionID
	content    string
}

const (
	storeAnswerDefaultSectionsPerDoc = 3
	storeAnswerEvidenceBudget        = 14000 // chars of evidence sent to the reducer
	storeAnswerQuoteLen              = 240
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
	if h.multiDoc == nil || h.llm == nil {
		writeErr(w, http.StatusNotImplemented, "store answers require an LLM-backed configuration")
		return
	}
	perDoc := body.MaxSectionsPerDoc
	if perDoc <= 0 {
		perDoc = storeAnswerDefaultSectionsPerDoc
	}
	model := body.Model
	if model == "" {
		model = h.llmModel
	}

	started := time.Now()

	// ── MAP ───────────────────────────────────────────────────────
	budget := retrieval.ContextBudget{ModelName: body.Model, MaxTokens: 100000, ReservedForPrompt: 4000, MaxParallelCalls: 8}
	md, err := h.multiDoc.Query(r.Context(), orgID, storeID(r), body.DocumentIDs, body.Query, budget)
	if err != nil {
		h.logger.Error("answer/store: map failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "store retrieval failed: "+err.Error())
		return
	}

	evidence, matched := h.gatherEvidence(r.Context(), md, perDoc)
	totalUsage := md.TotalUsage

	// No document surfaced anything relevant → honest refusal.
	if len(evidence) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"query":                  body.Query,
			"answer":                 "The documents in this store do not appear to address this query.",
			"citations":              []any{},
			"documents_searched":     len(body.DocumentIDs),
			"documents_with_matches": 0,
			"confidence":             0.0,
			"usage":                  usageMap(totalUsage),
			"elapsed_ms":             time.Since(started).Milliseconds(),
		})
		return
	}

	// ── REDUCE ────────────────────────────────────────────────────
	answer, citedIdx, synthUsage, err := h.synthesise(r.Context(), model, body.Query, evidence, body.MaxTokens)
	if err != nil {
		h.logger.Error("answer/store: reduce failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "answer synthesis failed: "+err.Error())
		return
	}
	totalUsage.Add(synthUsage)

	citations := buildStoreCitations(evidence, citedIdx)

	writeJSON(w, http.StatusOK, map[string]any{
		"query":                  body.Query,
		"answer":                 answer,
		"citations":              citations,
		"documents_searched":     len(body.DocumentIDs),
		"documents_with_matches": matched,
		"usage":                  usageMap(totalUsage),
		"elapsed_ms":             time.Since(started).Milliseconds(),
	})
}

// gatherEvidence turns the per-document retrieval result into a flat,
// globally-indexed, source-labelled evidence list, newest-relevance
// first (documents that matched more sections lead), capped by the
// evidence char budget. Returns the evidence and the count of documents
// that contributed at least one block.
func (h *AnswerStoreHandler) gatherEvidence(ctx context.Context, md *retrieval.MultiDocResult, perDoc int) ([]evidenceBlock, int) {
	// Deterministic doc order: more selected sections first, then by id.
	type docEntry struct {
		id tree.DocumentID
		dr *retrieval.DocResult
	}
	docs := make([]docEntry, 0, len(md.Documents))
	for id, dr := range md.Documents {
		docs = append(docs, docEntry{id, dr})
	}
	sort.Slice(docs, func(i, j int) bool {
		if len(docs[i].dr.SelectedIDs) != len(docs[j].dr.SelectedIDs) {
			return len(docs[i].dr.SelectedIDs) > len(docs[j].dr.SelectedIDs)
		}
		return docs[i].id < docs[j].id
	})

	var (
		out     []evidenceBlock
		spent   int
		matched int
		idx     = 1
	)
	for _, d := range docs {
		if d.dr.Tree == nil {
			continue
		}
		contributed := false
		for i, sid := range d.dr.SelectedIDs {
			if i >= perDoc || spent >= storeAnswerEvidenceBudget {
				break
			}
			sec := d.dr.Tree.FindByID(sid)
			if sec == nil || sec.ContentRef == "" {
				continue
			}
			content := h.loadContent(ctx, sec.ContentRef)
			if strings.TrimSpace(content) == "" {
				continue
			}
			remaining := storeAnswerEvidenceBudget - spent
			if len(content) > remaining {
				content = content[:remaining]
			}
			spent += len(content)
			out = append(out, evidenceBlock{
				index:      idx,
				docID:      d.id,
				docTitle:   d.dr.Tree.Title,
				startPage:  sec.PageStart,
				endPage:    sec.PageEnd,
				sectionIDs: []tree.SectionID{sid},
				content:    content,
			})
			idx++
			contributed = true
		}
		if contributed {
			matched++
		}
	}
	return out, matched
}

func (h *AnswerStoreHandler) loadContent(ctx context.Context, ref string) string {
	rc, _, err := h.storage.Get(ctx, ref)
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(raw)
}

// synthesise runs the single reducer LLM call. It returns the answer
// text (with [n] citation markers), the set of cited evidence indices,
// and usage.
func (h *AnswerStoreHandler) synthesise(ctx context.Context, model, query string, evidence []evidenceBlock, maxTokens int) (string, []int, retrieval.Usage, error) {
	var ev strings.Builder
	for _, b := range evidence {
		pages := formatPageRange(b.startPage, b.endPage)
		title := b.docTitle
		if title == "" {
			title = string(b.docID)
		}
		fmt.Fprintf(&ev, "[%d] %s", b.index, title)
		if pages != "" {
			fmt.Fprintf(&ev, " (%s)", pages)
		}
		ev.WriteString(":\n")
		ev.WriteString(strings.TrimSpace(b.content))
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
			{Role: llmgate.RoleUser, Content: fmt.Sprintf("QUESTION:\n%s\n\nEVIDENCE:\n%s\nReply with ONLY the JSON object.", query, ev.String())},
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

	answer, cited := parseStoreAnswer(resp.Content, len(evidence))
	return answer, cited, usage, nil
}

const storeAnswerSystemPrompt = `You answer a question using ONLY the supplied EVIDENCE, which is drawn from several documents in a collection. Each evidence block is prefixed with a number in brackets, e.g. [1], [2].

RULES:
- Answer ONLY from the evidence. Never invent facts. If the evidence does not answer the question, say so plainly.
- Cite every claim inline with the bracketed number(s) of the evidence you used, e.g. "Consent must be freely given [1]." Cite the specific block(s) that support each statement.
- Synthesise across documents when the answer draws on more than one — the whole point is one coherent answer over the collection.
- Be concise and well-structured. Use short paragraphs or a short list.

Reply with EXACTLY one JSON object, no markdown fences:
{"answer":"<answer text with [n] citation markers>","cited":[<the evidence numbers you actually cited>]}`

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
	// Fallback: use the raw text and scrape [n] markers from it.
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

// buildStoreCitations turns the cited evidence indices into the response
// citations array, preserving cited order and carrying document id/title,
// page range, section ids, and a short verbatim quote.
func buildStoreCitations(evidence []evidenceBlock, cited []int) []map[string]any {
	byIdx := make(map[int]evidenceBlock, len(evidence))
	for _, b := range evidence {
		byIdx[b.index] = b
	}
	// If the model cited nothing parseable, fall back to citing all
	// evidence so the user still sees the sources the answer used.
	if len(cited) == 0 {
		for _, b := range evidence {
			cited = append(cited, b.index)
		}
	}
	out := make([]map[string]any, 0, len(cited))
	for _, n := range cited {
		b, ok := byIdx[n]
		if !ok {
			continue
		}
		c := map[string]any{
			"index":       b.index,
			"document_id": b.docID,
			"section_ids": b.sectionIDs,
			"quote":       snippet(b.content, storeAnswerQuoteLen),
		}
		if b.docTitle != "" {
			c["document_title"] = b.docTitle
		}
		if b.startPage > 0 {
			c["start_page"] = b.startPage
			c["end_page"] = b.endPage
		}
		out = append(out, c)
	}
	return out
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
