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
	"sync"
	"time"

	"github.com/hallelx2/llmgate"
	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// storeTreeLoader is the narrow slice of *db.Pool the store handler needs
// to materialise each document's section tree. *db.Pool satisfies it.
type storeTreeLoader interface {
	LoadTree(ctx context.Context, docID tree.DocumentID, orgID, storeID string) (*tree.Tree, error)
}

// AnswerStoreHandler answers a question across a SET of documents (a
// "store" / collection) and synthesises a SINGLE grounded answer with
// per-section, per-document citations. It is reasoning-first over
// structure + summaries — the Vectorless primitive, no chunking, no
// embeddings:
//
//  1. reason over each document's STRUCTURE + SUMMARIES (its llms.txt-style
//     outline) to SELECT the sections whose full text is relevant.
//  2. fetch the FULL content of every selected relevant section.
//  3. GENERATE one answer from that full content, citing the sections and
//     documents it drew on.
//
// max_docs bounds how many documents are considered (0 = all). max_sections
// caps the total relevant sections whose full content is pulled (0 = all
// relevant). A content budget caps the total characters handed to the
// generator so a huge selection still fits the model context.
type AnswerStoreHandler struct {
	logger   *slog.Logger
	db       storeTreeLoader
	storage  storage.Storage
	llm      llmgate.Client
	llmModel string
}

// NewAnswerStoreHandler creates an AnswerStoreHandler. It returns 501 at
// request time when no LLM is configured.
func NewAnswerStoreHandler(
	logger *slog.Logger,
	db storeTreeLoader,
	store storage.Storage,
	llm llmgate.Client,
	llmModel string,
) *AnswerStoreHandler {
	return &AnswerStoreHandler{logger: logger, db: db, storage: store, llm: llm, llmModel: llmModel}
}

type answerStoreRequest struct {
	DocumentIDs []tree.DocumentID `json:"document_ids"`
	Query       string            `json:"query"`
	Model       string            `json:"model"`
	MaxDocs     int               `json:"max_docs"`     // cap documents considered; 0 = all
	MaxSections int               `json:"max_sections"` // cap relevant sections pulled; 0 = all
	MaxTokens   int               `json:"max_tokens"`
}

// relevantSection is one selected section with its full content, ready to
// hand to the generator.
type relevantSection struct {
	index        int // 1-based global citation index
	docID        tree.DocumentID
	docTitle     string
	sectionID    tree.SectionID
	sectionTitle string
	startPage    int
	endPage      int
	content      string
}

const (
	storeMapConcurrency  = 6
	storeContentBudget   = 40000 // total chars of full section content sent to the generator
	storeSectionCharsCap = 8000  // per-section content cap
	storeAnswerQuoteLen  = 240
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
	if h.llm == nil {
		writeErr(w, http.StatusNotImplemented, "store answers require an LLM-backed configuration")
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

	// ── 1+2. Per document: reason over structure+summaries → select
	// relevant sections → fetch their full content (parallel). ──────
	sections, docsMatched, mapUsage := h.selectRelevant(r.Context(), orgID, storeID(r), docIDs, body.Query, model, body.MaxSections)
	totalUsage := mapUsage

	if len(sections) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"query":                  body.Query,
			"answer":                 "No sections in this collection appear relevant to the query.",
			"citations":              []any{},
			"documents_searched":     len(docIDs),
			"documents_with_matches": 0,
			"sections_used":          0,
			"usage":                  usageMap(totalUsage),
			"elapsed_ms":             time.Since(started).Milliseconds(),
		})
		return
	}

	// ── 3. Generate one answer from the full section content. ──────
	answer, cited, synthUsage, err := h.generate(r.Context(), model, body.Query, sections, body.MaxTokens)
	if err != nil {
		h.logger.Error("answer/store: generate failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "answer generation failed: "+err.Error())
		return
	}
	totalUsage.Add(synthUsage)

	writeJSON(w, http.StatusOK, map[string]any{
		"query":                  body.Query,
		"answer":                 answer,
		"citations":              buildStoreCitations(sections, cited),
		"documents_searched":     len(docIDs),
		"documents_with_matches": docsMatched,
		"sections_used":          len(sections),
		"usage":                  usageMap(totalUsage),
		"elapsed_ms":             time.Since(started).Milliseconds(),
	})
}

// manifestRef maps a manifest index to a section id + whether it carries
// content directly.
type manifestRef struct {
	id         tree.SectionID
	hasContent bool
}

// selectRelevant runs, per document and in parallel: load tree → render
// the structure+summary outline → LLM-select the relevant sections →
// fetch their full content. It returns the flat list of relevant sections
// (globally indexed), the number of documents that contributed at least
// one section, and the summed selection usage. A total content budget
// (and optional max_sections) bounds the material.
func (h *AnswerStoreHandler) selectRelevant(ctx context.Context, orgID, storeID string, docIDs []tree.DocumentID, query, model string, maxSections int) ([]relevantSection, int, retrieval.Usage) {
	type docPick struct {
		secs  []relevantSection
		usage retrieval.Usage
	}
	var (
		mu    sync.Mutex
		picks = make(map[tree.DocumentID]docPick, len(docIDs))
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(storeMapConcurrency)
	for _, docID := range docIDs {
		g.Go(func() error {
			t, err := h.db.LoadTree(gctx, docID, orgID, storeID)
			if err != nil || t == nil || t.Root == nil {
				h.logger.Warn("answer/store: load tree", "doc", docID, "err", err)
				return nil
			}
			manifest, refs := buildSectionManifest(t.Root)
			if len(refs) == 0 {
				return nil
			}
			idxs, u, err := h.selectSections(gctx, model, query, t.Title, manifest, len(refs))
			if err != nil {
				h.logger.Warn("answer/store: select", "doc", docID, "err", err)
				return nil
			}

			// Resolve selected indices → content-bearing sections
			// (a selected internal node pulls its content leaves).
			var out []relevantSection
			seen := map[tree.SectionID]struct{}{}
			for _, n := range idxs {
				if n < 1 || n > len(refs) {
					continue
				}
				sec := t.FindByID(refs[n-1].id)
				if sec == nil {
					continue
				}
				for _, cs := range collectContentSections(sec) {
					if _, dup := seen[cs.ID]; dup {
						continue
					}
					seen[cs.ID] = struct{}{}
					content := h.loadContent(gctx, cs.ContentRef)
					if strings.TrimSpace(content) == "" {
						continue
					}
					out = append(out, relevantSection{
						docID:        docID,
						docTitle:     t.Title,
						sectionID:    cs.ID,
						sectionTitle: cs.Title,
						startPage:    cs.PageStart,
						endPage:      cs.PageEnd,
						content:      content,
					})
				}
			}

			mu.Lock()
			picks[docID] = docPick{secs: out, usage: u}
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	// Assemble in caller-supplied document order for determinism, apply
	// the total content budget + optional max_sections, and assign global
	// citation indices.
	var (
		flat    []relevantSection
		spent   int
		matched int
		usage   retrieval.Usage
		idx     = 1
	)
	for _, docID := range docIDs {
		p, ok := picks[docID]
		if !ok {
			continue
		}
		usage.Add(p.usage)
		if len(p.secs) == 0 {
			continue
		}
		matched++
		for _, s := range p.secs {
			if maxSections > 0 && len(flat) >= maxSections {
				break
			}
			if spent >= storeContentBudget {
				break
			}
			if len(s.content) > storeSectionCharsCap {
				s.content = s.content[:storeSectionCharsCap]
			}
			if remaining := storeContentBudget - spent; len(s.content) > remaining {
				s.content = s.content[:remaining]
			}
			spent += len(s.content)
			s.index = idx
			idx++
			flat = append(flat, s)
		}
	}
	return flat, matched, usage
}

// selectSections asks the model, over a document's structure+summary
// outline (NOT its full text), which sections' full content is relevant.
func (h *AnswerStoreHandler) selectSections(ctx context.Context, model, query, docTitle, manifest string, maxIdx int) ([]int, retrieval.Usage, error) {
	req := llmgate.Request{
		Model:       model,
		MaxTokens:   256,
		Temperature: 0,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: storeSelectSystemPrompt},
			{Role: llmgate.RoleUser, Content: fmt.Sprintf("QUESTION:\n%s\n\nDOCUMENT: %s\nSECTION OUTLINE (number, title, summary):\n%s\nReply with ONLY the JSON object.", query, docTitle, manifest)},
		},
	}
	resp, err := h.llm.Complete(ctx, req)
	if err != nil {
		return nil, retrieval.Usage{}, err
	}
	u := retrieval.Usage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		CostUSD:      resp.Usage.CostUSD,
		LLMCalls:     1,
	}
	return parseRelevant(resp.Content, maxIdx), u, nil
}

const storeSelectSystemPrompt = `You are given a QUESTION and the OUTLINE of ONE document — its sections, each with a number in brackets [n], a title, and a one-line summary. You are NOT given the section text.

Identify which sections' FULL text you would need to read to answer the question. Choose by relevance, favouring precision: pick the sections that actually bear on the question, not every loosely related one. If none are relevant, return an empty list.

Reply with EXACTLY one JSON object, no markdown fences:
{"relevant":[<the section numbers you would read>]}`

// generate produces the final answer from the FULL content of the
// selected sections, citing the sections it uses with [n] markers.
func (h *AnswerStoreHandler) generate(ctx context.Context, model, query string, sections []relevantSection, maxTokens int) (string, []int, retrieval.Usage, error) {
	var ev strings.Builder
	for _, s := range sections {
		title := s.docTitle
		if title == "" {
			title = string(s.docID)
		}
		fmt.Fprintf(&ev, "[%d] %s", s.index, title)
		if s.sectionTitle != "" {
			fmt.Fprintf(&ev, " › %s", s.sectionTitle)
		}
		if p := formatPageRange(s.startPage, s.endPage); p != "" {
			fmt.Fprintf(&ev, " (%s)", p)
		}
		ev.WriteString(":\n")
		ev.WriteString(strings.TrimSpace(s.content))
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
			{Role: llmgate.RoleSystem, Content: storeGenerateSystemPrompt},
			{Role: llmgate.RoleUser, Content: fmt.Sprintf("QUESTION:\n%s\n\nSECTIONS:\n%s\nReply with ONLY the JSON object.", query, ev.String())},
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
	answer, cited := parseStoreAnswer(resp.Content, len(sections))
	return answer, cited, usage, nil
}

const storeGenerateSystemPrompt = `You answer a QUESTION using ONLY the supplied SECTIONS, which are the full text of the relevant sections drawn from several documents in a collection. Each section is prefixed with a number in brackets [n] and its document title / page range.

RULES:
- Answer ONLY from the sections. Never invent facts. If they do not answer the question, say so plainly.
- Cite every claim inline with the bracketed number(s) of the section(s) it rests on, e.g. "Consent must be freely given [1]."
- Synthesise across documents when the answer draws on more than one — one coherent answer over the collection.
- Be concise and well-structured.

Reply with EXACTLY one JSON object, no markdown fences:
{"answer":"<answer text with [n] citation markers>","cited":[<the section numbers you cited>]}`

// buildSectionManifest renders a document's structure+summary outline as
// a numbered, indented list and returns the render plus the ordered
// index→section refs. Every non-root section gets a number; the LLM
// selects by number and we map back via refs.
func buildSectionManifest(root *tree.Section) (string, []manifestRef) {
	var (
		b    strings.Builder
		refs []manifestRef
	)
	var walk func(s *tree.Section, depth int)
	walk = func(s *tree.Section, depth int) {
		for _, c := range s.Children {
			refs = append(refs, manifestRef{id: c.ID, hasContent: strings.TrimSpace(c.ContentRef) != ""})
			n := len(refs)
			title := strings.TrimSpace(c.Title)
			if title == "" {
				title = string(c.ID)
			}
			fmt.Fprintf(&b, "[%d]%s %s", n, strings.Repeat("  ", depth), title)
			if pr := formatPageRange(c.PageStart, c.PageEnd); pr != "" {
				fmt.Fprintf(&b, " (%s)", pr)
			}
			if sum := strings.Join(strings.Fields(c.Summary), " "); sum != "" {
				fmt.Fprintf(&b, " — %s", sum)
			}
			b.WriteByte('\n')
			walk(c, depth+1)
		}
	}
	walk(root, 0)
	return b.String(), refs
}

// collectContentSections returns the content-bearing sections rooted at
// sec: sec itself if it has content, plus any descendants that do. A
// selected heading therefore pulls the full text of its subsection.
func collectContentSections(sec *tree.Section) []*tree.Section {
	var out []*tree.Section
	var walk func(s *tree.Section)
	walk = func(s *tree.Section) {
		if strings.TrimSpace(s.ContentRef) != "" {
			out = append(out, s)
		}
		for _, c := range s.Children {
			walk(c)
		}
	}
	walk(sec)
	return out
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

// ── output parsing + citation building ─────────────────────────────

type relevantJSON struct {
	Relevant []int `json:"relevant"`
}

// parseRelevant extracts the selected section numbers from the selector's
// reply, tolerant of code fences / prose, clamped to [1,maxIdx].
func parseRelevant(raw string, maxIdx int) []int {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			var parsed relevantJSON
			if err := json.Unmarshal([]byte(s[i:j+1]), &parsed); err == nil {
				return dedupeInRange(parsed.Relevant, maxIdx)
			}
		}
	}
	return dedupeInRange(scrapeMarkers(s, maxIdx), maxIdx)
}

type storeAnswerJSON struct {
	Answer string `json:"answer"`
	Cited  []int  `json:"cited"`
}

// parseStoreAnswer extracts the answer + cited indices from the generator
// reply, tolerant of code fences / prose. Falls back to the raw text (and
// scraped [n] markers) so a formatting slip never drops the answer.
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

// buildStoreCitations turns cited section indices into the response
// citations — one per cited section, carrying document + section
// provenance and a short quote from the section's full content.
func buildStoreCitations(sections []relevantSection, cited []int) []map[string]any {
	byIdx := make(map[int]relevantSection, len(sections))
	for _, s := range sections {
		byIdx[s.index] = s
	}
	if len(cited) == 0 { // generator cited nothing parseable → show all sources
		for _, s := range sections {
			cited = append(cited, s.index)
		}
	}
	out := make([]map[string]any, 0, len(cited))
	for _, n := range cited {
		s, ok := byIdx[n]
		if !ok {
			continue
		}
		c := map[string]any{
			"index":       s.index,
			"document_id": s.docID,
			"section_ids": []tree.SectionID{s.sectionID},
			"quote":       snippet(s.content, storeAnswerQuoteLen),
		}
		if s.docTitle != "" {
			c["document_title"] = s.docTitle
		}
		if s.sectionTitle != "" {
			c["section_title"] = s.sectionTitle
		}
		if s.startPage > 0 {
			c["start_page"] = s.startPage
			c["end_page"] = s.endPage
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
