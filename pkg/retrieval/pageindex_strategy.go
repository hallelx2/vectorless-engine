package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// PageIndexStrategy is a page-based agentic retrieval loop modelled on
// PageIndex's three-tool reasoning protocol.
//
// The model navigates by PAGE RANGE rather than by section ID. Each
// turn it emits one of:
//
//   - get_document_structure() — returns the document's TOC tree
//     (titles + page ranges only, no text), so the model can pick
//     which pages to look at.
//   - get_pages(start_page, end_page) — returns the concatenated text
//     of every section whose [page_start, page_end] overlaps the
//     requested range, clipped to PageContentLimit chars.
//   - done(answer, cited_pages, reasoning) — terminates with the final
//     answer string and the list of page ranges the answer relies on.
//
// This is a SUPERSET of the older AgenticStrategy's protocol: the
// loop owns the answer, not just the selection. SelectWithCost
// surfaces both the picked section IDs (the intersection of every
// cited page range with the document's section map) and the literal
// answer string via Result.Reasoning. The /v1/answer/pageindex
// endpoint reads the answer; the legacy /v1/query callers still get
// a section list.
//
// # Protocol choice
//
// PageIndex's original demo wires the model via the OpenAI Agents
// SDK's native tool-calling surface. llmgate v0.2.0 declares ToolDef
// / ToolCall as scaffolding but does not populate ToolCalls on
// responses, so this strategy uses the same JSON-action text
// protocol AgenticStrategy already proved (see pkg/retrieval/agentic.go).
// When llmgate wires native tool calling the surface here is the
// same — only the request/response plumbing changes.
type PageIndexStrategy struct {
	// LLM is the shared client used for every turn.
	LLM llmgate.Client

	// TOC is the source for get_document_structure observations.
	// Implementations read documents.toc_tree (the column PR-A adds)
	// or synthesise a tree from the section list. Nil triggers the
	// built-in fallback that mirrors the section tree.
	TOC TOCProvider

	// PageLoader materialises section content for get_pages
	// observations. Nil disables the get_pages tool — the model
	// would then only see structure observations.
	PageLoader PageContentLoader

	// MaxHops caps the number of LLM turns one Select consumes,
	// including the terminal "done" turn. Zero means use
	// defaultPageIndexMaxHops.
	MaxHops int

	// PageContentLimit caps how many chars a single get_pages
	// observation returns. Zero means use defaultPageContentLimit.
	// Limits like this keep one stray request from torching the
	// context window: a 50-page get_pages on an SEC filing can
	// easily blow past 200K chars otherwise.
	PageContentLimit int

	// ModelOverride, if non-empty, replaces the budget's ModelName
	// for every turn. Useful for routing the navigation loop to a
	// cheaper or faster model than the rest of the engine.
	ModelOverride string
}

// defaultPageIndexMaxHops bounds the loop. Eight turns is enough for
// structure → 3 get_pages → done with two retry hops on stray bad
// JSON, while keeping latency and cost predictable. The reference
// PageIndex demo converges in 3-5 hops on typical questions.
const defaultPageIndexMaxHops = 8

// defaultPageContentLimit is the per-call chars cap. 16,000 chars
// is roughly 4K tokens at GPT/Claude tokenisers — comfortably below
// any flagship model's context but enough text for a 5-7 page
// excerpt. Matches PageIndex's reference behaviour.
const defaultPageContentLimit = 16000

// strategyNamePageIndex is the stable identifier for config
// (retrieval.strategy: pageindex) and telemetry.
const strategyNamePageIndex = "pageindex"

// Compile-time interface checks.
var (
	_ Strategy     = (*PageIndexStrategy)(nil)
	_ CostStrategy = (*PageIndexStrategy)(nil)
)

// TOCProvider returns a JSON document-structure tree for the LLM's
// get_document_structure tool. Implementations should return a
// pretty-printable JSON array/object representing titles + page
// ranges. Nodes that carry full text MUST be stripped before return —
// the model is supposed to navigate by structure first and pull text
// only via get_pages.
//
// Returning (nil, ErrNoTOC) signals "no TOC available; fall back to
// the synthesised view". Other errors propagate.
type TOCProvider interface {
	GetTOC(ctx context.Context, docID tree.DocumentID) ([]byte, error)
}

// PageContentLoader returns the raw content bytes for one section,
// keyed by its ContentRef. Strategies that need to materialise text
// at run-time depend on this rather than on a concrete storage
// driver — same shape as ContentFetcher; we keep them distinct so
// the two callers (agentic / pageindex) can be wired independently
// in main.go.
type PageContentLoader interface {
	Load(ctx context.Context, ref string) ([]byte, error)
}

// ErrNoTOC signals that no LLM-built TOC tree has been persisted for
// the document yet. The strategy treats it as a graceful-degrade
// signal: it synthesises a TOC view from the section list rather
// than failing the request. Pre-merge of PR-A (which adds
// documents.toc_tree) every request will degrade through this path.
var ErrNoTOC = fmt.Errorf("retrieval: no TOC tree persisted for document")

// NewPageIndexStrategy constructs a PageIndexStrategy with sensible
// defaults. The TOC + PageLoader are nil here; the engine wires them
// in main.go from the DB pool + storage backend. Tests pass scripted
// implementations directly.
func NewPageIndexStrategy(client llmgate.Client) *PageIndexStrategy {
	return &PageIndexStrategy{
		LLM:              client,
		MaxHops:          defaultPageIndexMaxHops,
		PageContentLimit: defaultPageContentLimit,
	}
}

// Name implements Strategy.
func (s *PageIndexStrategy) Name() string { return strategyNamePageIndex }

// Select implements Strategy.
func (s *PageIndexStrategy) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	r, err := s.SelectWithCost(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return r.SelectedIDs, nil
}

// SelectWithCost implements CostStrategy.
//
// The returned Result populates:
//
//   - SelectedIDs: section IDs whose [PageStart,PageEnd] overlaps any
//     cited page range. This keeps the per-section-id contract for
//     callers (/v1/query, /v1/answer) that don't yet know about pages.
//   - Reasoning: the agent's final answer string (the "answer" field
//     of the done action). /v1/answer/pageindex reads this directly
//     and skips synthesis.
//   - PagesRead: an entry per get_pages call.
//   - HopsTaken / Usage / TraceToken: standard.
func (s *PageIndexStrategy) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	if t == nil || t.Root == nil {
		return &Result{}, nil
	}

	model := s.ModelOverride
	if model == "" {
		model = budget.ModelName
	}
	maxHops := s.MaxHops
	if maxHops <= 0 {
		maxHops = defaultPageIndexMaxHops
	}
	pageLimit := s.PageContentLimit
	if pageLimit <= 0 {
		pageLimit = defaultPageContentLimit
	}

	// Pre-flatten the tree into an ordinal section list ordered by
	// page. The get_pages observation iterates this twice per call;
	// pre-computing keeps the inner loop O(N) instead of O(N · depth).
	sections := flattenSectionsByPage(t)
	maxPage := maxKnownPage(sections)

	msgs := []llmgate.Message{
		{Role: llmgate.RoleSystem, Content: pageIndexSystemPrompt},
		{Role: llmgate.RoleUser, Content: s.initialUserPrompt(t, query, maxPage)},
	}

	var (
		totalUsage Usage
		hopsTaken  int
		pagesRead  []PageReadEntry

		// finalAnswer / finalCitedPages / finalReasoning are populated
		// when the model emits a done action. citedRanges drives the
		// final SelectedIDs (section IDs overlapping any cited range).
		finalAnswer    string
		finalReasoning string
		citedRanges    []pageRange
	)

	for hop := 0; hop < maxHops; hop++ {
		req := llmgate.Request{
			Model:       model,
			Messages:    msgs,
			MaxTokens:   1536, // answers can be longer than agentic's selections
			Temperature: 0,
		}
		resp, err := s.LLM.Complete(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("pageindex hop %d: %w", hop+1, err)
		}
		hopsTaken++
		totalUsage.Add(Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.TotalTokens,
			CostUSD:      resp.Usage.CostUSD,
			LLMCalls:     1,
		})

		// Record the assistant turn before parsing so the next prompt
		// has the model's own context (matches AgenticStrategy).
		msgs = append(msgs, llmgate.Message{
			Role:    llmgate.RoleAssistant,
			Content: resp.Content,
		})

		action, parseErr := ParsePageIndexAction(resp.Content)
		if parseErr != nil {
			log.Printf("retrieval: pageindex hop %d action parse failed: %v", hop+1, parseErr)
			msgs = append(msgs, llmgate.Message{
				Role:    llmgate.RoleUser,
				Content: pageIndexParseRetryPrompt,
			})
			continue
		}

		switch action.Action {
		case pageActionDone:
			finalAnswer = strings.TrimSpace(action.Answer)
			finalReasoning = strings.TrimSpace(action.Reasoning)
			citedRanges = normaliseRanges(action.CitedPages, maxPage)
			selectedIDs := sectionsOverlapping(sections, citedRanges)
			_ = finalReasoning // the answer-string is what callers consume
			return &Result{
				SelectedIDs: selectedIDs,
				Reasoning:   finalAnswer, // /v1/answer/pageindex reads this
				ModelUsed:   model,
				Usage:       totalUsage,
				HopsTaken:   hopsTaken,
				PagesRead:   pagesRead,
				TraceToken:  computePageIndexTraceToken(t.DocumentID, model, citedRanges),
			}, nil

		case pageActionStructure:
			obs := s.renderStructure(ctx, t)
			msgs = append(msgs, llmgate.Message{
				Role:    llmgate.RoleUser,
				Content: wrapPageObservation("get_document_structure", obs),
			})

		case pageActionGetPages:
			start, end, ok := clampRange(action.StartPage, action.EndPage, maxPage)
			if !ok {
				msgs = append(msgs, llmgate.Message{
					Role: llmgate.RoleUser,
					Content: wrapPageObservation("get_pages",
						fmt.Sprintf("invalid range start=%d end=%d (document has %d pages). Pages are 1-indexed inclusive.",
							action.StartPage, action.EndPage, maxPage)),
				})
				continue
			}
			text, sectionIDs := s.renderPages(ctx, sections, start, end, pageLimit)
			pagesRead = append(pagesRead, PageReadEntry{
				StartPage:  start,
				EndPage:    end,
				SectionIDs: sectionIDs,
				CharCount:  len(text),
			})
			msgs = append(msgs, llmgate.Message{
				Role: llmgate.RoleUser,
				Content: wrapPageObservation("get_pages",
					fmt.Sprintf("pages %d-%d (%d sections, %d chars):\n%s", start, end, len(sectionIDs), len(text), text)),
			})

		default:
			msgs = append(msgs, llmgate.Message{
				Role: llmgate.RoleUser,
				Content: wrapPageObservation(action.Action,
					fmt.Sprintf("unsupported tool %q. Use one of: get_document_structure, get_pages, done.", action.Action)),
			})
		}
	}

	// Ran out of hops without a done action. Force a terminal turn:
	// give the model one final chance with an explicit "you MUST emit
	// done now" prompt. If that also fails to parse or the model
	// ignores the rule, we return whatever pages have been read so
	// the caller at least sees the navigation footprint and an empty
	// answer rather than a 500.
	finalAnswer, finalReasoning, citedRanges = s.forceDone(ctx, &msgs, &totalUsage, &hopsTaken, model, maxPage)
	selectedIDs := sectionsOverlapping(sections, citedRanges)
	log.Printf("retrieval: pageindex strategy hit max_hops=%d; forced done", maxHops)
	_ = finalReasoning
	return &Result{
		SelectedIDs: selectedIDs,
		Reasoning:   finalAnswer,
		ModelUsed:   model,
		Usage:       totalUsage,
		HopsTaken:   hopsTaken,
		PagesRead:   pagesRead,
		TraceToken:  computePageIndexTraceToken(t.DocumentID, model, citedRanges),
	}, nil
}

// initialUserPrompt is the very first user turn. It explains the
// task, tells the model which page range exists ("the document has N
// pages"), and reminds it of the action protocol. Mirrors
// AgenticStrategy.initialUserPrompt.
func (s *PageIndexStrategy) initialUserPrompt(t *tree.Tree, query string, maxPage int) string {
	var b strings.Builder
	if t.Title != "" {
		b.WriteString("Document: ")
		b.WriteString(t.Title)
		b.WriteString("\n")
	}
	if maxPage > 0 {
		fmt.Fprintf(&b, "Pages: 1-%d (inclusive)\n", maxPage)
	} else {
		b.WriteString("Pages: unknown (this document carries no page metadata; rely on get_document_structure for navigation hints).\n")
	}
	b.WriteString("\nUser query:\n")
	b.WriteString(query)
	b.WriteString("\n\nReply with a JSON action. The tools you may use are:\n")
	b.WriteString(pageIndexActionHelp)
	return b.String()
}

// renderStructure produces the get_document_structure observation.
// First tries the persisted TOC tree (PR-A's documents.toc_tree
// JSONB); if that's nil or errors, falls back to a synthesised view
// derived from the section list. The fallback keeps this strategy
// useful even before PR-A merges.
func (s *PageIndexStrategy) renderStructure(ctx context.Context, t *tree.Tree) string {
	if s.TOC != nil {
		raw, err := s.TOC.GetTOC(ctx, t.DocumentID)
		if err == nil && len(raw) > 0 {
			return string(raw)
		}
		// Log and degrade — the strategy must keep going.
		if err != nil {
			log.Printf("retrieval: pageindex TOC fetch failed (degrading to synthesised view): %v", err)
		}
	}
	return synthesiseTOC(t)
}

// renderPages assembles the get_pages observation: concatenates the
// content of every section whose page range overlaps [start, end],
// clipped to pageLimit. Returns the rendered text plus the list of
// section IDs that contributed, in page order. SectionIDs feeds back
// into the PageReadEntry so callers can audit which sections the
// model actually read.
func (s *PageIndexStrategy) renderPages(ctx context.Context, sections []sectionPageEntry, start, end, pageLimit int) (string, []tree.SectionID) {
	if s.PageLoader == nil {
		// Without a loader we can still emit a useful observation
		// from titles + summaries, so the model can keep navigating.
		return s.renderPagesNoLoader(sections, start, end, pageLimit)
	}

	var (
		b          strings.Builder
		sectionIDs []tree.SectionID
		written    int
	)
	for _, sec := range sections {
		if !overlaps(sec.start, sec.end, start, end) {
			continue
		}
		sectionIDs = append(sectionIDs, sec.id)

		// Header line so the model can ground its citations to a
		// specific section + page range.
		header := fmt.Sprintf("\n--- section_id=%s title=%q pages=%d-%d ---\n", sec.id, sec.title, sec.start, sec.end)
		remaining := pageLimit - written
		if remaining <= 0 {
			break
		}
		if len(header) > remaining {
			b.WriteString(header[:remaining])
			written += remaining
			break
		}
		b.WriteString(header)
		written += len(header)

		// Body — preferred source: storage via PageLoader. Fall back
		// to the section summary when there's no ContentRef (internal
		// nodes) or the loader errors.
		body := s.loadSectionBody(ctx, sec)
		remaining = pageLimit - written
		if remaining <= 0 {
			break
		}
		if len(body) > remaining {
			b.WriteString(body[:remaining])
			written += remaining
			break
		}
		b.WriteString(body)
		written += len(body)
	}
	return b.String(), sectionIDs
}

// renderPagesNoLoader is the degraded-mode get_pages observation
// used when the strategy has no PageLoader (e.g. in tests, or when
// storage is wired but momentarily unavailable). Titles + summaries
// still let the model triangulate which range to ask about next.
func (s *PageIndexStrategy) renderPagesNoLoader(sections []sectionPageEntry, start, end, pageLimit int) (string, []tree.SectionID) {
	var (
		b          strings.Builder
		sectionIDs []tree.SectionID
	)
	for _, sec := range sections {
		if !overlaps(sec.start, sec.end, start, end) {
			continue
		}
		sectionIDs = append(sectionIDs, sec.id)
		fmt.Fprintf(&b, "section_id=%s title=%q pages=%d-%d summary=%q\n", sec.id, sec.title, sec.start, sec.end, sec.summary)
		if b.Len() >= pageLimit {
			break
		}
	}
	out := b.String()
	if len(out) > pageLimit {
		out = out[:pageLimit]
	}
	return out, sectionIDs
}

func (s *PageIndexStrategy) loadSectionBody(ctx context.Context, sec sectionPageEntry) string {
	if sec.contentRef == "" {
		if sec.summary != "" {
			return fmt.Sprintf("(summary, no content loaded)\n%s", sec.summary)
		}
		return ""
	}
	data, err := s.PageLoader.Load(ctx, sec.contentRef)
	if err != nil {
		log.Printf("retrieval: pageindex load failed for section %s: %v", sec.id, err)
		if sec.summary != "" {
			return fmt.Sprintf("(content load failed: %v; using summary)\n%s", err, sec.summary)
		}
		return fmt.Sprintf("(content load failed: %v)", err)
	}
	return strings.TrimSpace(string(data))
}

// forceDone runs one final hop with a hard "emit done NOW" prompt so
// the loop can exit gracefully on a stubborn model. Returns
// (answer, reasoning, cited_ranges). When the model still doesn't
// emit a valid done action, the empty values flow back and the
// caller sees a hop-capped Result.
func (s *PageIndexStrategy) forceDone(ctx context.Context, msgs *[]llmgate.Message, totalUsage *Usage, hopsTaken *int, model string, maxPage int) (string, string, []pageRange) {
	*msgs = append(*msgs, llmgate.Message{
		Role:    llmgate.RoleUser,
		Content: "You have used your tool-call budget. Reply NOW with one JSON object: {\"tool\":\"done\",\"answer\":\"<your best answer\",\"cited_pages\":[[start,end],...],\"reasoning\":\"why\"}. Do not call any more tools. Do not emit prose.",
	})
	req := llmgate.Request{
		Model:       model,
		Messages:    *msgs,
		MaxTokens:   1536,
		Temperature: 0,
	}
	resp, err := s.LLM.Complete(ctx, req)
	if err != nil {
		return "", "", nil
	}
	*hopsTaken++
	totalUsage.Add(Usage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		CostUSD:      resp.Usage.CostUSD,
		LLMCalls:     1,
	})
	action, err := ParsePageIndexAction(resp.Content)
	if err != nil || action.Action != pageActionDone {
		return "", "", nil
	}
	return strings.TrimSpace(action.Answer), strings.TrimSpace(action.Reasoning), normaliseRanges(action.CitedPages, maxPage)
}

// --- TOC synthesis ---

// tocNode is the synthesised TOC shape. It deliberately mirrors what
// PR-A's persisted documents.toc_tree column will store (titles +
// page ranges + nested children, NO text bodies) so the LLM sees a
// consistent surface whether PR-A is merged or not.
type tocNode struct {
	ID        tree.SectionID `json:"id,omitempty"`
	Title     string         `json:"title"`
	PageStart int            `json:"page_start,omitempty"`
	PageEnd   int            `json:"page_end,omitempty"`
	Children  []tocNode      `json:"children,omitempty"`
}

// synthesiseTOC builds a TOC view from the section tree when no
// LLM-built TOC has been persisted. Titles + page ranges only — body
// text is intentionally dropped so the model navigates structure
// before reaching for get_pages.
func synthesiseTOC(t *tree.Tree) string {
	if t == nil || t.Root == nil {
		return "[]"
	}
	nodes := convertSectionToTOC(t.Root)
	raw, err := json.MarshalIndent(nodes, "", "  ")
	if err != nil {
		// json.Marshal on a static struct shouldn't fail, but
		// degrade to an empty array rather than break the loop.
		return "[]"
	}
	return string(raw)
}

func convertSectionToTOC(s *tree.Section) []tocNode {
	if s == nil {
		return nil
	}
	// Root with empty ID is the synthetic wrapper buildTree adds when
	// a document has multiple top-level sections — surface its
	// children directly so the TOC doesn't look like one big anonymous
	// container.
	if s.ID == "" {
		var out []tocNode
		for _, c := range s.Children {
			out = append(out, sectionToTOC(c))
		}
		return out
	}
	return []tocNode{sectionToTOC(s)}
}

func sectionToTOC(s *tree.Section) tocNode {
	n := tocNode{
		ID:        s.ID,
		Title:     s.Title,
		PageStart: s.PageStart,
		PageEnd:   s.PageEnd,
	}
	for _, c := range s.Children {
		n.Children = append(n.Children, sectionToTOC(c))
	}
	return n
}

// --- page range maths ---

// pageRange is an inclusive page range. The strategy uses it for
// citations and for the trace-token input.
type pageRange struct {
	Start int
	End   int
}

// String formats as "start-end" so trace tokens compute over a stable
// human-readable form. computePageIndexTraceToken sorts these
// strings before hashing.
func (p pageRange) String() string {
	if p.Start == p.End {
		return strconv.Itoa(p.Start)
	}
	return fmt.Sprintf("%d-%d", p.Start, p.End)
}

// sectionPageEntry is a flat section view ordered by page. The
// strategy keeps the title + summary inline because get_pages /
// renderPages reads them both per call, and the fallback path needs
// summaries when the loader is nil.
type sectionPageEntry struct {
	id         tree.SectionID
	title      string
	summary    string
	contentRef string
	start      int
	end        int
}

// flattenSectionsByPage walks the tree and returns every section
// that carries a page range, sorted by start page (ties broken by
// end page). Internal nodes whose [start,end] is zero are dropped —
// they don't contribute content to get_pages and would noise up the
// overlap check.
func flattenSectionsByPage(t *tree.Tree) []sectionPageEntry {
	if t == nil || t.Root == nil {
		return nil
	}
	var out []sectionPageEntry
	t.Root.Walk(func(s *tree.Section) bool {
		if s.PageStart <= 0 || s.PageEnd <= 0 {
			return true
		}
		out = append(out, sectionPageEntry{
			id:         s.ID,
			title:      s.Title,
			summary:    s.Summary,
			contentRef: s.ContentRef,
			start:      s.PageStart,
			end:        s.PageEnd,
		})
		return true
	})
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end < out[j].end
	})
	return out
}

// maxKnownPage returns the highest PageEnd across the flattened
// section list. Used to clamp model-emitted ranges and to give the
// model a clear "max page = N" hint in the initial prompt.
func maxKnownPage(sections []sectionPageEntry) int {
	max := 0
	for _, s := range sections {
		if s.end > max {
			max = s.end
		}
	}
	return max
}

// overlaps reports whether two inclusive ranges intersect.
func overlaps(aStart, aEnd, bStart, bEnd int) bool {
	if aStart <= 0 || aEnd <= 0 || bStart <= 0 || bEnd <= 0 {
		return false
	}
	return aStart <= bEnd && bStart <= aEnd
}

// clampRange validates a model-emitted [start,end] against the
// document's actual page range. Returns (start, end, ok=false) when
// the range is unusable (zero pages, inverted, or entirely past the
// document). When the range partially overlaps the document the ends
// are clamped to [1, maxPage] and the call returns ok=true so the
// model can keep navigating from a slightly-corrected range rather
// than spinning on the same error.
func clampRange(start, end, maxPage int) (int, int, bool) {
	if start <= 0 && end <= 0 {
		return 0, 0, false
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 {
		end = start
	}
	if start > end {
		start, end = end, start
	}
	if maxPage > 0 {
		if start > maxPage {
			return start, end, false
		}
		if end > maxPage {
			end = maxPage
		}
	}
	return start, end, true
}

// normaliseRanges collapses raw model-emitted ranges (which may be
// flipped, zero-pages, or duplicated) into a sorted, deduplicated
// list of valid inclusive ranges clamped to [1,maxPage]. Bad ranges
// are silently dropped — the trace token must compute over a stable
// canonical form regardless of how the model orders its citations.
func normaliseRanges(raw [][2]int, maxPage int) []pageRange {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[pageRange]struct{}, len(raw))
	out := make([]pageRange, 0, len(raw))
	for _, r := range raw {
		s, e, ok := clampRange(r[0], r[1], maxPage)
		if !ok {
			continue
		}
		pr := pageRange{Start: s, End: e}
		if _, dup := seen[pr]; dup {
			continue
		}
		seen[pr] = struct{}{}
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End < out[j].End
	})
	return out
}

// sectionsOverlapping returns the IDs of every section whose
// [PageStart, PageEnd] overlaps any of the cited ranges. Preserves
// document order (because sections is page-sorted) and deduplicates.
// This is the bridge that turns the model's page-based citations
// into the section-ID list every other endpoint already expects.
func sectionsOverlapping(sections []sectionPageEntry, ranges []pageRange) []tree.SectionID {
	if len(ranges) == 0 || len(sections) == 0 {
		return nil
	}
	seen := make(map[tree.SectionID]struct{}, len(sections))
	out := make([]tree.SectionID, 0, len(sections))
	for _, sec := range sections {
		for _, r := range ranges {
			if overlaps(sec.start, sec.end, r.Start, r.End) {
				if _, dup := seen[sec.id]; !dup {
					seen[sec.id] = struct{}{}
					out = append(out, sec.id)
				}
				break
			}
		}
	}
	return out
}

// computePageIndexTraceToken builds the replay token for a
// PageIndex run. Page-based strategies don't pick section IDs the
// way agentic/single-pass do, so the token's "identity" inputs are
// the document, the model, and the sorted cited page ranges. Two
// runs that cite the same pages (even via different navigation
// paths) collapse to the same token — same property as
// ComputeTraceToken offers for section IDs.
//
// The hashing primitive (sha256, NUL separators, lowercase hex) is
// reused so /v1/replay handles both shapes uniformly.
func computePageIndexTraceToken(docID tree.DocumentID, model string, ranges []pageRange) string {
	strs := make([]string, len(ranges))
	for i, r := range ranges {
		strs[i] = r.String()
	}
	sort.Strings(strs)
	// Trace-token IDs are constructed from sorted page-range strings
	// rather than section IDs. We feed them through the existing
	// ComputeTraceToken helper for shape consistency — its
	// sort-then-hash semantics happens to be exactly what we want
	// here too. The strategy's stable identifier ("pageindex") is
	// folded into the "model" position so a page-based run and a
	// section-based run on the same doc/model don't collide.
	tagged := make([]tree.SectionID, len(strs))
	for i, s := range strs {
		tagged[i] = tree.SectionID("p:" + s)
	}
	return ComputeTraceToken(docID, traceDocVersionV1+"-pages", strategyNamePageIndex+":"+model, tagged)
}

// --- action protocol ---

// PageIndexAction is the LLM-chosen next step in the loop. The model
// emits one of these per turn as a JSON object on the
// 'tool' tag. The Action field is uppercase-tolerant on input;
// ParsePageIndexAction lowercases before dispatch.
type PageIndexAction struct {
	// Action is the dispatch tag (alias: tool). One of:
	// get_document_structure, get_pages, done.
	Action string `json:"tool"`

	// ActionAlt lets the model use "action" instead of "tool". Some
	// providers struggle to consistently emit the same key when both
	// shapes are documented. We accept either; ActionAlt wins iff
	// Action is empty.
	ActionAlt string `json:"action,omitempty"`

	// StartPage / EndPage are the inclusive 1-indexed range a
	// get_pages call targets.
	StartPage int `json:"start_page,omitempty"`
	EndPage   int `json:"end_page,omitempty"`

	// Pages is an alternate shape some models reach for: a
	// "5-7"-style string. ParsePageIndexAction splits it into
	// StartPage/EndPage when present.
	Pages string `json:"pages,omitempty"`

	// Answer is the natural-language answer for a done action.
	Answer string `json:"answer,omitempty"`

	// CitedPages is the list of inclusive page ranges the answer
	// relies on for a done action. Each entry is [start, end]; a
	// single page can be expressed as [5,5].
	CitedPages [][2]int `json:"cited_pages,omitempty"`

	// Reasoning is the per-call explanation the system prompt
	// asks the model to emit. Surfaced into the reasoning_trace
	// when the endpoint is called with ?reasoning=true.
	Reasoning string `json:"reasoning,omitempty"`
}

// Action tag constants. Mirrors PageIndex's reference SDK tool
// names so prompt-engineering work over there translates over.
const (
	pageActionStructure = "get_document_structure"
	pageActionGetPages  = "get_pages"
	pageActionDone      = "done"
)

// pageIndexParseRetryPrompt nudges the model back onto the
// JSON-action protocol after a parse failure. Aligned with
// AgenticStrategy's retry path — same wording so behaviour stays
// consistent.
const pageIndexParseRetryPrompt = "Your last reply was not a valid JSON tool call. Reply with EXACTLY one JSON object: {\"tool\":\"get_document_structure|get_pages|done\", ...}. No prose, no markdown fences."

// ParsePageIndexAction is the tolerant JSON decoder for the
// page-based protocol. Behaviour mirrors ParseAction (the older
// agentic protocol's parser): strip code fences, peel prose
// wrappers, isolate the first balanced JSON object, and
// case-fold the action tag.
//
// Additional tolerance vs ParseAction:
//   - "tool" or "action" can name the action.
//   - Pages can be a "5-7" string instead of explicit
//     start_page/end_page.
//   - cited_pages can be either [[5,7],[10,10]] (preferred) or
//     ["5-7","10"] (tolerated).
func ParsePageIndexAction(raw string) (PageIndexAction, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return PageIndexAction{}, fmt.Errorf("empty response")
	}
	if strings.HasPrefix(raw, "```") {
		if i := strings.Index(raw, "\n"); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if j := strings.LastIndex(raw, "}"); j >= 0 && j < len(raw)-1 {
		raw = raw[:j+1]
	}

	// We decode in two passes so a flexibly-typed cited_pages field
	// (either [[1,2],[5,7]] or ["1-2","5-7"]) doesn't tank the whole
	// action.
	//
	// Pass 1: decode into a map[string]json.RawMessage so each field
	// can be parsed independently. This is more tolerant than a
	// single-pass typed decode because a single bad field doesn't
	// invalidate the rest of the JSON.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return PageIndexAction{}, fmt.Errorf("decode pageindex action: %w", err)
	}

	var a PageIndexAction
	if v, ok := fields["tool"]; ok {
		_ = json.Unmarshal(v, &a.Action)
	}
	if a.Action == "" {
		if v, ok := fields["action"]; ok {
			_ = json.Unmarshal(v, &a.Action)
		}
	}
	a.Action = strings.ToLower(strings.TrimSpace(a.Action))
	if a.Action == "" {
		return PageIndexAction{}, fmt.Errorf("missing 'tool' or 'action' field")
	}

	if v, ok := fields["start_page"]; ok {
		_ = json.Unmarshal(v, &a.StartPage)
	}
	if v, ok := fields["end_page"]; ok {
		_ = json.Unmarshal(v, &a.EndPage)
	}
	if v, ok := fields["pages"]; ok {
		_ = json.Unmarshal(v, &a.Pages)
	}
	if v, ok := fields["answer"]; ok {
		_ = json.Unmarshal(v, &a.Answer)
	}
	if v, ok := fields["reasoning"]; ok {
		_ = json.Unmarshal(v, &a.Reasoning)
	}

	// cited_pages: try the typed shape first ([[1,2],[5,7]]); fall
	// back to the string-shape (["1-2","5-7"]) when the typed
	// decode fails or is empty.
	if v, ok := fields["cited_pages"]; ok && len(v) > 0 {
		if err := json.Unmarshal(v, &a.CitedPages); err != nil || len(a.CitedPages) == 0 {
			a.CitedPages = nil
			var asStrings []string
			if err := json.Unmarshal(v, &asStrings); err == nil {
				for _, p := range asStrings {
					s, e, ok := parsePageRangeString(p)
					if !ok {
						continue
					}
					a.CitedPages = append(a.CitedPages, [2]int{s, e})
				}
			}
		}
	}

	// Pages-string → start/end normalisation. Only fills in when
	// the typed fields weren't already populated.
	if a.Pages != "" && a.StartPage == 0 && a.EndPage == 0 {
		s, e, ok := parsePageRangeString(a.Pages)
		if ok {
			a.StartPage = s
			a.EndPage = e
		}
	}

	return a, nil
}

// parsePageRangeString parses "5", "5-7", or "5,7" (the loosest
// shape the model is allowed to emit). Returns (start, end, true)
// on success; (0, 0, false) otherwise. "5,7" is treated as
// start=5,end=7 (we don't support multi-range here — that's what
// cited_pages is for).
func parsePageRangeString(s string) (int, int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	sep := -1
	for i, c := range s {
		if c == '-' || c == ',' {
			sep = i
			break
		}
	}
	if sep < 0 {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return n, n, true
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(s[:sep]))
	b, err2 := strconv.Atoi(strings.TrimSpace(s[sep+1:]))
	if err1 != nil || err2 != nil || a <= 0 || b <= 0 {
		return 0, 0, false
	}
	return a, b, true
}

// wrapPageObservation formats a tool's result so the model can
// clearly see which tool produced which observation. Same shape as
// AgenticStrategy.wrapObservation but with tool-call wording.
func wrapPageObservation(tool, body string) string {
	return fmt.Sprintf("Tool result (%s):\n%s\n\nNext JSON tool call?", tool, body)
}

// --- system prompt ---

// pageIndexSystemPrompt instructs the model on the navigation loop.
// The wording is a faithful port of the reference PageIndex demo's
// AGENT_SYSTEM_PROMPT (see PageIndex/examples/agentic_vectorless_rag_demo.py:44-52),
// adapted to the JSON-action protocol vle uses in lieu of native
// llmgate tool calling.
//
// Key invariants that show up in tests:
//   - Always call get_document_structure first.
//   - Use tight page ranges; never fetch the whole document.
//   - Emit a one-sentence reason before each tool call.
//   - Answer only from tool output (no priors).
//   - End with a done action carrying answer + cited_pages.
const pageIndexSystemPrompt = `You are a document QA assistant navigating a paginated document.

TOOL USE PROTOCOL:
- Reply with EXACTLY one JSON object per turn. No prose, no markdown fences.
- Always call get_document_structure first to see titles + page ranges.
- Call get_pages with TIGHT page ranges (e.g. {"tool":"get_pages","start_page":5,"end_page":7}). Never fetch the whole document.
- Before each tool call, populate the "reasoning" field with ONE short sentence explaining why you're calling it.
- When you have enough evidence, emit done with the natural-language answer, the page ranges you relied on, and a one-line reasoning trace.

RULES:
- Answer based ONLY on tool output. Do not invent facts.
- Cite by page range, not by section title.
- Be concise. Single-paragraph answers when possible.
- If nothing in the document answers the query, emit done with answer="The document does not address this query." and an empty cited_pages array.`

// pageIndexActionHelp is the one-shot reminder appended to the
// initial user prompt so the model gets concrete examples without us
// needing to maintain a separate few-shot block.
const pageIndexActionHelp = `- {"tool":"get_document_structure","reasoning":"orient by titles"} — fetch the TOC tree (titles + page ranges, no body text)
- {"tool":"get_pages","start_page":5,"end_page":7,"reasoning":"section on debt"} — fetch text covering pages 5-7
- {"tool":"done","answer":"...","cited_pages":[[5,7],[12,12]],"reasoning":"the answer is grounded on these pages"} — final answer

Reply with ONLY the JSON object.`
