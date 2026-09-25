package retrieval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"regexp"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Retrieval navigation on a Judge — select, don't generate (HAL-1371).
//
// TreeWalkStrategy navigates by running a chat model for up to eight
// hops, each carrying the structure and every page read so far. But
// navigation is two judgements over candidate sets the tree already
// provides: which sections could hold the answer, then which pages in
// them do. A Judge answers each set in one batched request. The
// generative model, if any, is then asked one question over the
// evidence pages only — and never asked to navigate.

// NavLeaf is one navigable section: a leaf of the TOC tree with pages.
type NavLeaf struct {
	ID      string
	Title   string
	Path    string // "PART II > Item 7. Management's Discussion…"
	Start   int
	End     int
	Summary string // optional; shown to the Judge when present
}

// NavPage is one unit of text the page ranking judges: a real page, or
// a chunk of a section body when pages are not individually addressable.
type NavPage struct {
	Number int
	Text   string
	LeafID string
}

// LeafScore is the Judge's probability that a leaf holds what the
// question needs.
type LeafScore struct {
	Leaf NavLeaf
	P    float64
}

// PageScore is the Judge's probability that a page contains it.
type PageScore struct {
	Page NavPage
	P    float64
}

// NavResult is what navigation found.
type NavResult struct {
	Leaves   []LeafScore // every leaf, best first
	Selected []NavLeaf   // the leaves whose pages were read
	Coarse   []PageScore // the coarse pass over page heads, when it ran; best first
	Pages    []PageScore // every page read in full, best first
	Evidence []PageScore // pages above threshold, best first; never empty when any page was read
	Followed []NavLeaf   // leaves read because an evidence page referred to them
	Usage    Usage
	Requests int
}

// JudgeNavigator ranks leaves, then pages, on a Judge.
type JudgeNavigator struct {
	Judge llmgate.Judge

	// Threshold is the probability at or above which a page counts as
	// evidence. Zero selects 0.5.
	Threshold float64

	// MaxLeaves bounds how many sections' pages are gathered. Zero
	// means the page budget alone decides: sections are taken in rank
	// order until CoarsePages are gathered. A fixed count of five was
	// right for a 23-leaf 10-K and wrong for one split into 80 —
	// five one-page notes and the budget went unused (HAL-1374).
	MaxLeaves int

	// MaxPages bounds how many pages are judged in full. Zero selects 40.
	MaxPages int

	// CoarsePages bounds how many gathered pages the coarse pass may
	// rank by their heads before the best MaxPages are read in full.
	// Zero selects 120. A 10-K's Item 8 alone is 70 pages; without the
	// coarse pass the page budget cut it off at 40 and the statement on
	// page 113 was never read.
	CoarsePages int

	// HeadChars is how much of a page the coarse pass sees. Zero selects
	// 700 — the heading and the first rows of a table.
	HeadChars int

	// FollowReferences, when true (the default), takes one more hop when
	// the evidence pages point elsewhere — "see Note 21" — and the leaf
	// they point to has not been read. A 10-K's Item 3 is one sentence
	// that refers to the note where the legal proceedings actually are.
	NoFollowReferences bool

	// PageChars truncates each page's text before the Judge sees it.
	// Zero selects 6000 — a dense filing page is ~4–5k characters.
	PageChars int

	// RequestBudgetTokens bounds the STATE of one page-ranking request.
	// The provider treats the whole state object as shared context for
	// every question — there is no per-question state — so state plus
	// the longest question must stay under 32k tokens. Zero selects
	// defaultNavReqTokens.
	//
	// This is a LATENCY knob, not just a limit. Batches are sent
	// concurrently, so a smaller budget means more requests in flight,
	// not more waiting — and the provider is far happier with many small
	// requests than a few large ones (see defaultNavReqTokens).
	RequestBudgetTokens int
}

const (
	defaultNavThreshold = 0.5
	defaultNavMaxPages  = 40
	defaultNavCoarse    = 120
	defaultNavHeadChars = 700
	defaultNavPageChars = 6000
	// defaultNavReqTokens bounds one request's state. Measured warm on
	// 2026-09-25, one Noul per page: request SHAPE barely matters.
	// Forty pages went out as ten parallel requests in 2.6 s wall, and
	// as sixteen-page requests in 2.6 s each. An earlier probe appeared
	// to punish large requests; that was cold-start contamination — the
	// first calls of a session take 5-6 s and the rest settle to
	// 1.5-2.6 s.
	//
	// This was briefly retuned to 6k on the strength of that bad probe
	// and one navigation run came back three times slower (not a paired
	// control either). No measured reason to move off 24k, which stays
	// under the provider's 32k per-question ceiling with room for the
	// question text.
	defaultNavReqTokens  = 24_000
	navLeafBatch         = 120
	navMinEvidencePages  = 2
	navLeafStateMaxChars = 300
)

func (n *JudgeNavigator) threshold() float64 {
	if n.Threshold > 0 {
		return n.Threshold
	}
	return defaultNavThreshold
}

func (n *JudgeNavigator) maxLeaves() int {
	if n.MaxLeaves > 0 {
		return n.MaxLeaves
	}
	return 1 << 30 // the page budget governs
}

func (n *JudgeNavigator) maxPages() int {
	if n.MaxPages > 0 {
		return n.MaxPages
	}
	return defaultNavMaxPages
}

func (n *JudgeNavigator) coarsePages() int {
	if n.CoarsePages > 0 {
		return n.CoarsePages
	}
	return defaultNavCoarse
}

func (n *JudgeNavigator) headChars() int {
	if n.HeadChars > 0 {
		return n.HeadChars
	}
	return defaultNavHeadChars
}

func (n *JudgeNavigator) pageChars() int {
	if n.PageChars > 0 {
		return n.PageChars
	}
	return defaultNavPageChars
}

func (n *JudgeNavigator) reqTokens() int {
	if n.RequestBudgetTokens > 0 {
		return n.RequestBudgetTokens
	}
	return defaultNavReqTokens
}

// RankLeaves asks, for every leaf, whether the section is where the
// question's answer would be found. One request per 120 leaves; the
// query is shared state, each leaf is ~40 tokens of question state.
func (n *JudgeNavigator) RankLeaves(ctx context.Context, query string, leaves []NavLeaf) ([]LeafScore, Usage, int, error) {
	var usage Usage
	if n.Judge == nil {
		return nil, usage, 0, fmt.Errorf("judgewalk: no Judge configured")
	}
	scores := make([]LeafScore, len(leaves))
	for i, l := range leaves {
		scores[i] = LeafScore{Leaf: l}
	}
	requests := 0
	for start := 0; start < len(leaves); start += navLeafBatch {
		end := start + navLeafBatch
		if end > len(leaves) {
			end = len(leaves)
		}
		state := map[string]any{"question": query}
		questions := map[string]llmgate.Question{}
		for i := start; i < end; i++ {
			l := leaves[i]
			qk := fmt.Sprintf("l_%d", i)
			item := map[string]any{"title": l.Title, "pages": fmt.Sprintf("%d-%d", l.Start, l.End)}
			if l.Path != "" && l.Path != l.Title {
				item["path"] = l.Path
			}
			if s := strings.TrimSpace(l.Summary); s != "" {
				if len(s) > navLeafStateMaxChars {
					s = s[:navLeafStateMaxChars]
				}
				item["summary"] = s
			}
			state[qk] = item
			questions[qk] = llmgate.Noul{
				Instructions: fmt.Sprintf(
					"`question` is a question about a document. `%s` is one section of that "+
						"document's table of contents: its title, where it sits, and its page range. "+
						"Would the information needed to answer the question be found in this "+
						"section? Judge from what such a section of such a document contains: "+
						"figures live in the statements and their notes, discussion of results in "+
						"the MD&A, customers, competition and outlook in the business and risk "+
						"sections.", qk),
				Criteria: &llmgate.NoulCriteria{
					True:  "This section is where a reader would look for the answer",
					False: "The answer would not be in this section",
				},
			}
		}
		res, err := n.Judge.Judge(ctx, llmgate.JudgeRequest{State: state, Questions: questions})
		if err != nil {
			return nil, usage, requests, err
		}
		requests++
		usage.Add(judgeUsage(res))
		for qk := range questions {
			p, err := res.Noul(qk)
			if err != nil {
				continue
			}
			var i int
			fmt.Sscanf(qk, "l_%d", &i)
			scores[i].P = p
		}
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].P > scores[j].P })
	return scores, usage, requests, nil
}

// RankPages asks, for every page, whether it contains the facts or
// figures the question asks for. Pages are batched so the request's
// whole state — query plus every page in the batch — stays under the
// budget, counted with the provider's tokenizer.
func (n *JudgeNavigator) RankPages(ctx context.Context, query string, pages []NavPage) ([]PageScore, Usage, int, error) {
	return n.rankPages(ctx, query, pages, n.pageChars(), false)
}

// rankPages is RankPages with the text window chosen by the caller:
// the full page, or just its head for the coarse pass.
func (n *JudgeNavigator) rankPages(ctx context.Context, query string, pages []NavPage, limit int, coarse bool) ([]PageScore, Usage, int, error) {
	var usage Usage
	if n.Judge == nil {
		return nil, usage, 0, fmt.Errorf("judgewalk: no Judge configured")
	}
	scores := make([]PageScore, len(pages))
	for i, p := range pages {
		scores[i] = PageScore{Page: p}
	}
	// Build every batch first, then send them all at once. They do not
	// depend on each other, and the provider's limiter — not a loop —
	// decides how many are in flight (HAL-1372).
	type batch struct {
		state     map[string]any
		questions map[string]llmgate.Question
	}
	var batches []batch
	budget := n.reqTokens()
	for start := 0; start < len(pages); {
		state := map[string]any{"question": query}
		questions := map[string]llmgate.Question{}
		used := countTokens(query) + 100
		end := start
		for end < len(pages) {
			text := pages[end].Text
			if len(text) > limit {
				text = text[:limit]
			}
			cost := countTokens(text) + 20
			if end > start && used+cost > budget {
				break
			}
			qk := fmt.Sprintf("p_%d", end)
			state[qk] = map[string]any{"page": pages[end].Number, "text": text}
			if coarse {
				questions[qk] = llmgate.Noul{
					Instructions: fmt.Sprintf(
						"`question` is a question about a document. `%s.text` is the START of page "+
							"`%s.page` — its heading and first lines. Could the facts or figures needed "+
							"to answer the question be on this page, judging from what it opens with?", qk, qk),
					Criteria: &llmgate.NoulCriteria{
						True:  "This page's opening says it is the kind of page that would hold the answer",
						False: "This page opens on something unrelated",
					},
				}
			} else {
				questions[qk] = llmgate.Noul{
					Instructions: fmt.Sprintf(
						"`question` is a question about a document. `%s.text` is the text of page "+
							"`%s.page`. Does this page contain the specific facts or figures needed to "+
							"answer the question — the number, the statement, the table row? A page that "+
							"only mentions the topic, or refers the reader elsewhere, does not.", qk, qk),
					Criteria: &llmgate.NoulCriteria{
						True:  "The answer, or a figure it is computed from, is on this page",
						False: "The page is about something else, or only mentions the topic",
					},
				}
			}
			used += cost
			end++
		}
		batches = append(batches, batch{state, questions})
		start = end
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		requests int
	)
	for _, b := range batches {
		wg.Add(1)
		go func(b batch) {
			defer wg.Done()
			res, err := n.Judge.Judge(ctx, llmgate.JudgeRequest{State: b.state, Questions: b.questions})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			requests++
			usage.Add(judgeUsage(res))
			for qk := range b.questions {
				p, err := res.Noul(qk)
				if err != nil {
					continue
				}
				var i int
				fmt.Sscanf(qk, "p_%d", &i)
				scores[i].P = p
			}
		}(b)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, usage, requests, firstErr
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].P > scores[j].P })
	return scores, usage, requests, nil
}

// Navigate ranks the leaves, reads the pages of the best few, ranks
// those pages, and returns the evidence set. loadPages returns the
// pages (or body chunks) of one leaf.
func (n *JudgeNavigator) Navigate(ctx context.Context, query string, leaves []NavLeaf, loadPages func(ctx context.Context, leaf NavLeaf) ([]NavPage, error)) (*NavResult, error) {
	out := &NavResult{}
	if len(leaves) == 0 {
		return out, nil
	}
	ranked, u, r, err := n.RankLeaves(ctx, query, leaves)
	if err != nil {
		return nil, fmt.Errorf("judgewalk: rank leaves: %w", err)
	}
	out.Usage.Add(u)
	out.Requests += r
	out.Leaves = ranked

	// Gather the pages of the best leaves, in rank order, up to the
	// coarse budget. A leaf below threshold is still read when nothing
	// better exists: a low-confidence best guess beats reading nothing.
	// A page two overlapping leaves both cover is gathered once.
	var pages []NavPage
	seen := map[int]bool{}
	coarseCap := n.coarsePages()
	for _, ls := range ranked {
		if len(out.Selected) >= n.maxLeaves() || len(pages) >= coarseCap {
			break
		}
		ps, err := loadPages(ctx, ls.Leaf)
		if err != nil {
			return nil, fmt.Errorf("judgewalk: load %q: %w", ls.Leaf.Title, err)
		}
		added := 0
		for _, p := range ps {
			if seen[p.Number] || len(pages) >= coarseCap {
				continue
			}
			seen[p.Number] = true
			p.LeafID = ls.Leaf.ID
			pages = append(pages, p)
			added++
		}
		if added > 0 {
			out.Selected = append(out.Selected, ls.Leaf)
		}
	}
	if len(pages) == 0 {
		return out, nil
	}

	// More pages than the full-text budget: a coarse pass over page
	// heads picks which ones deserve their whole text. Heads are
	// ~150 tokens, so 120 of them is one request.
	maxP := n.maxPages()
	if len(pages) > maxP {
		heads, u, r, err := n.rankPages(ctx, query, pages, n.headChars(), true)
		if err != nil {
			return nil, fmt.Errorf("judgewalk: rank page heads: %w", err)
		}
		out.Usage.Add(u)
		out.Requests += r
		out.Coarse = heads
		pages = pages[:0]
		for _, h := range heads[:maxP] {
			pages = append(pages, h.Page)
		}
		sort.Slice(pages, func(i, j int) bool { return pages[i].Number < pages[j].Number })
	}
	scored, u, r, err := n.RankPages(ctx, query, pages)
	if err != nil {
		return nil, fmt.Errorf("judgewalk: rank pages: %w", err)
	}
	out.Usage.Add(u)
	out.Requests += r
	out.Pages = scored
	th := n.threshold()
	fill := func(scored []PageScore) {
		out.Evidence = out.Evidence[:0]
		for _, ps := range scored {
			if ps.P >= th {
				out.Evidence = append(out.Evidence, ps)
			}
		}
		// Never come back empty-handed: the best pages are the evidence,
		// flagged by their probability.
		if len(out.Evidence) < navMinEvidencePages {
			out.Evidence = append(out.Evidence[:0], scored[:min(navMinEvidencePages, len(scored))]...)
		}
	}
	fill(scored)

	// One more hop when the evidence points elsewhere. Code reads the
	// reference, the tree names the leaf, the Judge reads its pages.
	if !n.NoFollowReferences {
		var refPages []NavPage
		readLeaf := map[string]bool{}
		for _, l := range out.Selected {
			readLeaf[l.ID] = true
		}
		for _, ev := range out.Evidence {
			for _, target := range referencedLeaves(ev.Page.Text, leaves) {
				if readLeaf[target.ID] {
					continue
				}
				readLeaf[target.ID] = true
				ps, err := loadPages(ctx, target)
				if err != nil {
					return nil, fmt.Errorf("judgewalk: follow %q: %w", target.Title, err)
				}
				for _, p := range ps {
					if !seen[p.Number] && len(refPages) < maxP {
						seen[p.Number] = true
						p.LeafID = target.ID
						refPages = append(refPages, p)
					}
				}
				out.Followed = append(out.Followed, target)
			}
		}
		if len(refPages) > 0 {
			more, u, r, err := n.RankPages(ctx, query, refPages)
			if err != nil {
				return nil, fmt.Errorf("judgewalk: rank referenced pages: %w", err)
			}
			out.Usage.Add(u)
			out.Requests += r
			out.Pages = append(out.Pages, more...)
			sort.SliceStable(out.Pages, func(i, j int) bool { return out.Pages[i].P > out.Pages[j].P })
			fill(out.Pages)
		}
	}
	return out, nil
}

// countTokens estimates a string's token count for packing batches.
//
// It deliberately does NOT run the provider's tokenizer. Measured
// 2026-09-25: tokenising one question's forty pages costs 4.0 s of
// CPU, and the client tokenises the assembled state again before every
// request — about five seconds per question spent counting rather than
// asking, on a query whose median is thirty-seven.
//
// The packing budget only needs a safe upper bound; the client's own
// check is exact and refuses anything over the ceiling.
//
// Measured on real filing pages: 24,000 characters of page text bill
// as 4,875 tokens, so 4.9 characters per token. len/2 was the first
// guess and over-estimated by two and a half times, halving every
// batch and sending 6.6 requests per question where 4.3 had done —
// which cancelled the CPU saved. len/4 leaves a fifth of headroom
// over the measured ratio, which covers dense numeric tables without
// throwing away batch size.
func countTokens(text string) int {
	return len(text)/4 + 1
}

// reReference finds the cross-references a page makes: "see Note 21",
// "Item 1A", "Note 14 to the Consolidated Financial Statements".
var reReference = regexp.MustCompile(`(?i)\b(note|item|part|section|schedule)\s+(\d+[a-c]?)\b`)

// referencedLeaves returns the leaves a page's cross-references name,
// matched by label and number at the start of the leaf's title.
func referencedLeaves(text string, leaves []NavLeaf) []NavLeaf {
	var out []NavLeaf
	seen := map[string]bool{}
	for _, m := range reReference.FindAllStringSubmatch(text, -1) {
		label, num := strings.ToLower(m[1]), strings.ToLower(m[2])
		key := label + " " + num
		if seen[key] {
			continue
		}
		seen[key] = true
		for _, l := range leaves {
			t := strings.ToLower(strings.TrimSpace(l.Title))
			// "Note 21. Legal Proceedings", "NOTE 21 — …", "Item 1A."
			if strings.HasPrefix(t, key) {
				rest := t[len(key):]
				if rest == "" || !(rest[0] >= '0' && rest[0] <= '9') && !(rest[0] >= 'a' && rest[0] <= 'z') {
					out = append(out, l)
					break
				}
			}
		}
	}
	return out
}

func judgeUsage(res *llmgate.Judgment) Usage {
	if res == nil {
		return Usage{}
	}
	return Usage{
		InputTokens:  res.Usage.InputTokens,
		OutputTokens: res.Usage.OutputTokens,
		TotalTokens:  res.Usage.TotalTokens,
		CostUSD:      res.Usage.CostUSD,
		LLMCalls:     1,
	}
}

// JudgeWalkStrategy is JudgeNavigator as a Strategy over a section
// tree. Leaves are the sections that carry a page range; a section's
// body, loaded through PageLoader, is chunked into page-sized units for
// the page ranking. The result names the sections the evidence came
// from and their page ranges. It does not write an answer: answering
// is the one generative step, and it belongs to the caller, over the
// evidence only.
type JudgeWalkStrategy struct {
	Navigator  JudgeNavigator
	PageLoader PageContentLoader

	// TOC and Pages, when both are set and both have data for the
	// document, are what navigation runs over: the table of contents
	// ingest built (leaves with real page ranges, sub-sections and all)
	// and the per-page text ingest persisted beside it. That is the
	// pipeline the FinanceBench evaluations measured. Without either,
	// navigation falls back to the section tree and section bodies —
	// the parser's page attribution, which is what HAL-1375 showed to
	// be unreliable — and scored 0.65 where the pages scored 0.90.
	TOC   TOCProvider
	Pages PageStore
}

// PageStore serves a document's per-page text, as ingest persisted it.
type PageStore interface {
	LoadPages(ctx context.Context, docID tree.DocumentID) ([]NavPage, error)
}

const strategyNameJudgeWalk = "judgewalk"

// NewJudgeWalkStrategy builds the strategy on a Judge.
func NewJudgeWalkStrategy(j llmgate.Judge) *JudgeWalkStrategy {
	return &JudgeWalkStrategy{Navigator: JudgeNavigator{Judge: j}}
}

func (s *JudgeWalkStrategy) Name() string { return strategyNameJudgeWalk }

// Select returns the section IDs the evidence pages came from.
func (s *JudgeWalkStrategy) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	res, err := s.SelectWithCost(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return res.SelectedIDs, nil
}

// SelectWithCost runs navigation and reports usage.
func (s *JudgeWalkStrategy) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	if t == nil || t.Root == nil {
		return &Result{}, nil
	}
	if res, ok := s.selectOnPersistedPages(ctx, t, query); ok {
		return res, nil
	}
	return s.selectOnSectionTree(ctx, t, query, budget)
}

// selectOnPersistedPages navigates the persisted table of contents over
// the persisted pages. ok is false when either is missing, so the
// caller falls back; an error from navigation itself is returned as a
// Result error through ok=true.
func (s *JudgeWalkStrategy) selectOnPersistedPages(ctx context.Context, t *tree.Tree, query string) (*Result, bool) {
	if s.TOC == nil || s.Pages == nil {
		return nil, false
	}
	raw, err := s.TOC.GetTOC(ctx, t.DocumentID)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	var nodes []tree.TOCNode
	if err := json.Unmarshal(raw, &nodes); err != nil || len(nodes) == 0 {
		return nil, false
	}
	pages, err := s.Pages.LoadPages(ctx, t.DocumentID)
	if err != nil || len(pages) == 0 {
		return nil, false
	}
	byNum := make(map[int]NavPage, len(pages))
	for _, p := range pages {
		byNum[p.Number] = p
	}
	var leaves []NavLeaf
	var walk func(ns []tree.TOCNode, path string)
	walk = func(ns []tree.TOCNode, path string) {
		for _, n := range ns {
			p := n.Title
			if path != "" {
				p = path + " > " + n.Title
			}
			if len(n.Nodes) > 0 {
				walk(n.Nodes, p)
				continue
			}
			if n.StartPage > 0 && n.EndPage >= n.StartPage {
				leaves = append(leaves, NavLeaf{ID: n.NodeID, Title: n.Title, Path: p, Start: n.StartPage, End: n.EndPage, Summary: n.Summary})
			}
		}
	}
	walk(nodes, "")
	if len(leaves) == 0 {
		return nil, false
	}
	load := func(_ context.Context, leaf NavLeaf) ([]NavPage, error) {
		var ps []NavPage
		for n := leaf.Start; n <= leaf.End; n++ {
			if p, ok := byNum[n]; ok {
				ps = append(ps, p)
			}
		}
		return ps, nil
	}
	nav, err := s.Navigator.Navigate(ctx, query, leaves, load)
	if err != nil {
		return &Result{ModelUsed: "judge"}, false
	}
	// The API answers in sections. Each evidence page names the sections
	// that cover it; the cited pages are the evidence pages themselves.
	sections := flattenSectionsByPage(t)
	var ranges []pageRange
	conf := map[tree.SectionID]float64{}
	var ids []tree.SectionID
	seen := map[tree.SectionID]bool{}
	for _, ev := range nav.Evidence {
		pg := ev.Page.Number
		ranges = append(ranges, pageRange{Start: pg, End: pg})
		for _, id := range sectionsOverlapping(sections, []pageRange{{Start: pg, End: pg}}) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
			if ev.P > conf[id] {
				conf[id] = ev.P
			}
		}
	}
	best := 0.0
	if len(nav.Evidence) > 0 {
		best = nav.Evidence[0].P
	}
	leafTitle := map[string]string{}
	for _, l := range leaves {
		leafTitle[l.ID] = l.Title
	}
	evidence := make([]EvidencePage, 0, len(nav.Evidence))
	for _, ev := range nav.Evidence {
		evidence = append(evidence, EvidencePage{Page: ev.Page.Number, Title: leafTitle[ev.Page.LeafID], Text: ev.Page.Text, Confidence: ev.P})
	}
	return &Result{
		SelectedIDs:   ids,
		Confidences:   conf,
		Confidence:    best,
		CitedPages:    rangesToPairs(ranges),
		EvidencePages: evidence,
		ModelUsed:     "judge",
		Usage:         nav.Usage,
		HopsTaken:     nav.Requests,
	}, true
}

// selectOnSectionTree is the fallback: the parser's section tree, each
// section's body chunked into page-sized units.
func (s *JudgeWalkStrategy) selectOnSectionTree(ctx context.Context, t *tree.Tree, query string, _ ContextBudget) (*Result, error) {
	sections := flattenSectionsByPage(t)
	byID := map[string]sectionPageEntry{}
	paths := sectionPaths(t)
	var leaves []NavLeaf
	for _, sec := range sections {
		byID[string(sec.id)] = sec
		leaves = append(leaves, NavLeaf{
			ID: string(sec.id), Title: sec.title, Path: paths[sec.id],
			Start: sec.start, End: sec.end, Summary: sec.summary,
		})
	}
	load := func(ctx context.Context, leaf NavLeaf) ([]NavPage, error) {
		sec := byID[leaf.ID]
		if s.PageLoader == nil || sec.contentRef == "" {
			return nil, nil
		}
		b, err := s.PageLoader.Load(ctx, sec.contentRef)
		if err != nil {
			return nil, err
		}
		return chunkBody(string(b), sec.start, sec.end, s.Navigator.pageChars()), nil
	}
	nav, err := s.Navigator.Navigate(ctx, query, leaves, load)
	if err != nil {
		return nil, err
	}
	var ids []tree.SectionID
	conf := map[tree.SectionID]float64{}
	var ranges []pageRange
	seen := map[string]bool{}
	for _, ev := range nav.Evidence {
		id := tree.SectionID(ev.Page.LeafID)
		if !seen[ev.Page.LeafID] {
			seen[ev.Page.LeafID] = true
			ids = append(ids, id)
			sec := byID[ev.Page.LeafID]
			ranges = append(ranges, pageRange{Start: sec.start, End: sec.end})
		}
		if ev.P > conf[id] {
			conf[id] = ev.P
		}
	}
	best := 0.0
	if len(nav.Evidence) > 0 {
		best = nav.Evidence[0].P
	}
	return &Result{
		SelectedIDs: ids,
		Confidences: conf,
		Confidence:  best,
		CitedPages:  rangesToPairs(ranges),
		ModelUsed:   "judge",
		Usage:       nav.Usage,
		HopsTaken:   nav.Requests,
	}, nil
}

// sectionPaths maps each section to its "parent > child" title path.
func sectionPaths(t *tree.Tree) map[tree.SectionID]string {
	out := map[tree.SectionID]string{}
	var walk func(s *tree.Section, prefix string)
	walk = func(s *tree.Section, prefix string) {
		p := s.Title
		if prefix != "" && s.Title != "" {
			p = prefix + " > " + s.Title
		} else if prefix != "" {
			p = prefix
		}
		out[s.ID] = p
		for i := range s.Children {
			walk(s.Children[i], p)
		}
	}
	if t != nil && t.Root != nil {
		for i := range t.Root.Children {
			walk(t.Root.Children[i], "")
		}
	}
	return out
}

// chunkBody splits a section body into page-sized units, numbered
// through the section's page range so a chunk can be cited by page.
func chunkBody(body string, start, end, size int) []NavPage {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if size <= 0 {
		size = defaultNavPageChars
	}
	var chunks []string
	for len(body) > size {
		cut := strings.LastIndex(body[:size], "\n")
		if cut < size/2 {
			cut = size
		}
		chunks = append(chunks, body[:cut])
		body = strings.TrimSpace(body[cut:])
	}
	if body != "" {
		chunks = append(chunks, body)
	}
	span := end - start + 1
	if span < 1 {
		span = 1
	}
	out := make([]NavPage, len(chunks))
	for i, c := range chunks {
		page := start + i*span/len(chunks)
		if page > end {
			page = end
		}
		out[i] = NavPage{Number: page, Text: c}
	}
	return out
}
