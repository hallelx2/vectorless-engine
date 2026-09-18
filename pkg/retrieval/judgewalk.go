package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
	Pages    []PageScore // every page read, best first
	Evidence []PageScore // pages above threshold, best first; never empty when any page was read
	Usage    Usage
	Requests int
}

// JudgeNavigator ranks leaves, then pages, on a Judge.
type JudgeNavigator struct {
	Judge llmgate.Judge

	// Threshold is the probability at or above which a page counts as
	// evidence. Zero selects 0.5.
	Threshold float64

	// MaxLeaves bounds how many sections' pages are read. Zero selects 3.
	MaxLeaves int

	// MaxPages bounds how many pages are judged in total. Zero selects 40.
	MaxPages int

	// PageChars truncates each page's text before the Judge sees it.
	// Zero selects 6000 — a dense filing page is ~4–5k characters.
	PageChars int

	// RequestBudgetTokens bounds one page-ranking request. Zero selects
	// 40k, under the provider's 64k request ceiling with room for the
	// shared query.
	RequestBudgetTokens int
}

const (
	defaultNavThreshold  = 0.5
	defaultNavMaxLeaves  = 3
	defaultNavMaxPages   = 40
	defaultNavPageChars  = 6000
	defaultNavReqTokens  = 40_000
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
	return defaultNavMaxLeaves
}

func (n *JudgeNavigator) maxPages() int {
	if n.MaxPages > 0 {
		return n.MaxPages
	}
	return defaultNavMaxPages
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
						"section? Judge from what such a section of such a document contains — a "+
						"financial statement holds the figures, an MD&A holds the discussion, a "+
						"risk-factors section holds neither.", qk),
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
// figures the question asks for. Pages are batched so one request stays
// under the budget; each page's text is its own question state, so the
// per-question ceiling is a page, not the sum.
func (n *JudgeNavigator) RankPages(ctx context.Context, query string, pages []NavPage) ([]PageScore, Usage, int, error) {
	var usage Usage
	if n.Judge == nil {
		return nil, usage, 0, fmt.Errorf("judgewalk: no Judge configured")
	}
	scores := make([]PageScore, len(pages))
	for i, p := range pages {
		scores[i] = PageScore{Page: p}
	}
	requests := 0
	budget := n.reqTokens()
	limit := n.pageChars()
	for start := 0; start < len(pages); {
		state := map[string]any{"question": query}
		questions := map[string]llmgate.Question{}
		used := len(query) / 4
		end := start
		for end < len(pages) {
			text := pages[end].Text
			if len(text) > limit {
				text = text[:limit]
			}
			cost := len(text)/4 + 60
			if end > start && used+cost > budget {
				break
			}
			qk := fmt.Sprintf("p_%d", end)
			state[qk] = map[string]any{"page": pages[end].Number, "text": text}
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
			used += cost
			end++
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
			fmt.Sscanf(qk, "p_%d", &i)
			scores[i].P = p
		}
		start = end
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

	// The best leaves, in rank order, until the page budget is spent.
	// A leaf below threshold is still read when nothing better exists:
	// a low-confidence best guess beats reading nothing.
	var pages []NavPage
	maxP := n.maxPages()
	for _, ls := range ranked {
		if len(out.Selected) >= n.maxLeaves() {
			break
		}
		ps, err := loadPages(ctx, ls.Leaf)
		if err != nil {
			return nil, fmt.Errorf("judgewalk: load %q: %w", ls.Leaf.Title, err)
		}
		if len(ps) == 0 {
			continue
		}
		for i := range ps {
			ps[i].LeafID = ls.Leaf.ID
		}
		if len(pages)+len(ps) > maxP {
			ps = ps[:maxP-len(pages)]
		}
		pages = append(pages, ps...)
		out.Selected = append(out.Selected, ls.Leaf)
		if len(pages) >= maxP {
			break
		}
	}
	if len(pages) == 0 {
		return out, nil
	}
	scored, u, r, err := n.RankPages(ctx, query, pages)
	if err != nil {
		return nil, fmt.Errorf("judgewalk: rank pages: %w", err)
	}
	out.Usage.Add(u)
	out.Requests += r
	out.Pages = scored
	th := n.threshold()
	for _, ps := range scored {
		if ps.P >= th {
			out.Evidence = append(out.Evidence, ps)
		}
	}
	// Never come back empty-handed: the best pages are the evidence,
	// flagged by their probability.
	if len(out.Evidence) < navMinEvidencePages {
		out.Evidence = scored[:min(navMinEvidencePages, len(scored))]
	}
	return out, nil
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
func (s *JudgeWalkStrategy) SelectWithCost(ctx context.Context, t *tree.Tree, query string, _ ContextBudget) (*Result, error) {
	if t == nil || t.Root == nil {
		return &Result{}, nil
	}
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
