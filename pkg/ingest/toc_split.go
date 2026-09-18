package ingest

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Leaf granularity (HAL-1374).
//
// A 10-K's tree is fine where nothing is and coarse where everything
// is: Items 1B, 2, 3 and 4 are a paragraph each, while Item 8 and its
// notes are 70 pages under one title. Retrieval pays for that — a
// 70-page leaf is a 40-page read to find one page — and a citation
// that says "Item 8" is not a citation.
//
// So a leaf whose span exceeds SplitLeavesOver is split into sub-leaves
// at its own internal headings, the same way the top level was built:
// code lists the candidates, the Judge confirms them. Two sources, in
// order of trust:
//
//  1. A nested contents page inside the leaf — Item 8 opens with an
//     "Index to the Consolidated Financial Statements" listing every
//     note with its page. Parsed, confirmed and resolved exactly like
//     the document's own contents page.
//  2. Failing that, heading-shaped lines in the leaf's pages: short,
//     line-opening, Title Case or capitals, not repeated on many pages
//     (a running header is not a heading). The Judge sees each with the
//     lines that follow it and says whether a sub-section starts there.

const (
	// splitMinEntries is how many confirmed sub-headings make a split
	// worth having. One is a title, not a structure.
	splitMinEntries = 2
	// splitMaxPerPage bounds sub-leaves to one per two pages: finer than
	// that is paragraphs, not sections.
	splitMaxPerPage = 0.5
	// splitHeadingMaxWords bounds a heading-shaped line.
	splitHeadingMaxWords = 9
	// splitRunningHeaderRepeats: a line seen on this many pages of one
	// leaf is furniture.
	splitRunningHeaderRepeats = 3
	// splitExcerptChars is the context shown with a candidate heading.
	splitExcerptChars = 320
)

var (
	reSplitNoise    = regexp.MustCompile(`(?i)^(table of contents|contents|page|index|continued|\(continued\)|notes? to (the )?consolidated financial statements|[\d,.$%()\s-]+)$`)
	reSplitNumbered = regexp.MustCompile(`(?i)^(note|item|section|part)\s+\d+[a-c]?\b`)
)

// splitLargeLeaves walks the tree and splits every leaf whose span
// exceeds over pages. It runs after end pages are derived, so spans are
// known, and re-derives them for the children it adds. Returns how many
// sub-leaves were added.
func (b *TOCBuilder) splitLargeLeaves(ctx context.Context, nodes []tree.TOCNode, pages []PageText, over int, usage *Usage) int {
	if over <= 0 || b.Judge == nil {
		return 0
	}
	byPage := map[int]string{}
	for _, p := range pages {
		byPage[p.PageNumber] = p.Text
	}
	added := 0
	var walk func(ns []tree.TOCNode)
	walk = func(ns []tree.TOCNode) {
		for i := range ns {
			n := &ns[i]
			if len(n.Nodes) > 0 {
				walk(n.Nodes)
				continue
			}
			if n.StartPage <= 0 || n.EndPage < n.StartPage || n.EndPage-n.StartPage+1 <= over {
				continue
			}
			subs, err := b.splitLeaf(ctx, n, pages, byPage, over, usage)
			if err != nil {
				log.Printf("toc: split %q failed, leaf kept whole: %v", n.Title, err)
				usage.degrade("leaf split", fmt.Sprintf("%q kept whole: %v", n.Title, err))
				continue
			}
			if len(subs) < splitMinEntries {
				continue
			}
			for j := range subs {
				subs[j].Structure = fmt.Sprintf("%s.%d", n.Structure, j+1)
			}
			deriveEndPagesIn(subs, n.EndPage)
			n.Nodes = subs
			added += len(subs)
		}
	}
	walk(nodes)
	return added
}

// splitLeaf returns the sub-leaves of one leaf, with start pages, in
// page order.
func (b *TOCBuilder) splitLeaf(ctx context.Context, leaf *tree.TOCNode, pages []PageText, byPage map[int]string, over int, usage *Usage) ([]tree.TOCNode, error) {
	var leafPages []PageText
	for _, p := range pages {
		if p.PageNumber >= leaf.StartPage && p.PageNumber <= leaf.EndPage {
			leafPages = append(leafPages, p)
		}
	}
	if len(leafPages) < 2 {
		return nil, nil
	}

	// 1. A nested contents page.
	if idx, entries := nestedContents(leafPages); idx > 0 {
		subs, err := b.subLeavesFromContents(ctx, leaf, entries, byPage[idx], leafPages, idx, usage)
		if err != nil {
			return nil, err
		}
		if len(subs) >= splitMinEntries {
			return subs, nil
		}
	}

	// 2. Heading-shaped lines.
	cands := headingCandidates(leafPages)
	if len(cands) < splitMinEntries {
		return nil, nil
	}
	return b.subLeavesFromHeadings(ctx, leaf, cands, over, usage)
}

// nestedContents finds a page inside the leaf that reads as a table of
// contents with at least three entries, and returns its number and
// entries. Zero when there is none.
func nestedContents(leafPages []PageText) (int, []contentsEntry) {
	for _, p := range leafPages {
		if !prefilterTOC(p.Text).Any() {
			continue
		}
		es := parseContentsEntries(p.Text)
		if countRealEntries(es) >= 3 {
			return p.PageNumber, es
		}
	}
	return 0, nil
}

// subLeavesFromContents confirms the nested contents' entries and
// resolves each within the leaf's pages, exactly as the top level does.
func (b *TOCBuilder) subLeavesFromContents(ctx context.Context, leaf *tree.TOCNode, entries []contentsEntry, contents string, leafPages []PageText, indexPage int, usage *Usage) ([]tree.TOCNode, error) {
	keep, err := b.confirmEntriesJudge(ctx, entries, contents, usage)
	if err != nil {
		return nil, err
	}
	var subs []tree.TOCNode
	for i, e := range entries {
		if !keep[i] || e.Container || normalise(e.Title) == normalise(leaf.Title) {
			continue // the leaf's own title is not a sub-section of it
		}
		subs = append(subs, tree.TOCNode{Title: e.Title})
	}
	if len(subs) < splitMinEntries {
		return nil, nil
	}
	resolved, handled, err := b.resolvePagesJudgeErr(ctx, subs, leafPages, []int{indexPage}, usage)
	if err != nil {
		return nil, err
	}
	if handled {
		applyResolvedPages(subs, resolved)
	}
	// Keep what was placed, in page order; a sub-section that could not
	// be found in the leaf's pages does not become a leaf that points
	// nowhere.
	var placed []tree.TOCNode
	for _, s := range subs {
		if s.StartPage >= leaf.StartPage && s.StartPage <= leaf.EndPage {
			placed = append(placed, s)
		}
	}
	sortByStart(placed)
	return capSubLeaves(placed, leaf), nil
}

// headingCandidate is a line that looks like a heading, with where it is.
type headingCandidate struct {
	text    string
	page    int
	excerpt string
}

// headingCandidates lists the heading-shaped lines of a leaf's pages,
// one per distinct heading, dropping lines that repeat across pages.
func headingCandidates(leafPages []PageText) []headingCandidate {
	seenOnPages := map[string]map[int]bool{}
	first := map[string]headingCandidate{}
	var order []string
	for _, p := range leafPages {
		lines := strings.Split(p.Text, "\n")
		for i, raw := range lines {
			line := strings.TrimSpace(strings.Trim(raw, "* \t"))
			if !looksLikeSubHeading(line) {
				continue
			}
			key := normalise(line)
			if seenOnPages[key] == nil {
				seenOnPages[key] = map[int]bool{}
			}
			seenOnPages[key][p.PageNumber] = true
			if _, ok := first[key]; !ok {
				rest := strings.Join(lines[i+1:], "\n")
				if len(rest) > splitExcerptChars {
					rest = rest[:splitExcerptChars]
				}
				first[key] = headingCandidate{text: line, page: p.PageNumber, excerpt: line + "\n" + rest}
				order = append(order, key)
			}
		}
	}
	var out []headingCandidate
	for _, key := range order {
		if len(seenOnPages[key]) >= splitRunningHeaderRepeats {
			continue // a running header, a repeated column label
		}
		out = append(out, first[key])
	}
	return out
}

// looksLikeSubHeading is the code-side filter: short, opens with a
// capital, Title Case or all capitals, no sentence punctuation, not
// page furniture. The Judge decides the rest.
func looksLikeSubHeading(line string) bool {
	if line == "" || len(line) > 90 {
		return false
	}
	if reSplitNoise.MatchString(line) {
		return false
	}
	if reSplitNumbered.MatchString(line) {
		return true
	}
	words := strings.Fields(line)
	if len(words) == 0 || len(words) > splitHeadingMaxWords {
		return false
	}
	if strings.HasSuffix(line, ".") || strings.HasSuffix(line, ",") || strings.HasSuffix(line, ";") || strings.HasSuffix(line, ":") {
		return false
	}
	r := []rune(words[0])[0]
	if !unicode.IsUpper(r) {
		return false
	}
	caps, lower := 0, 0
	for _, w := range words {
		if isStopword(strings.ToLower(w)) {
			continue
		}
		fr := []rune(w)[0]
		if unicode.IsUpper(fr) {
			caps++
		} else if unicode.IsLetter(fr) {
			lower++
		}
	}
	return caps > 0 && lower == 0
}

// subLeavesFromHeadings asks the Judge, for each candidate, whether a
// sub-section of the leaf begins at that line.
func (b *TOCBuilder) subLeavesFromHeadings(ctx context.Context, leaf *tree.TOCNode, cands []headingCandidate, over int, usage *Usage) ([]tree.TOCNode, error) {
	th := b.judgeThreshold()
	prob := make([]float64, len(cands))
	const perBatch = 80
	for start := 0; start < len(cands); start += perBatch {
		end := start + perBatch
		if end > len(cands) {
			end = len(cands)
		}
		state := map[string]any{"section": leaf.Title}
		questions := map[string]llmgate.Question{}
		for i := start; i < end; i++ {
			qk := fmt.Sprintf("h_%d", i)
			state[qk] = map[string]any{"line": cands[i].text, "passage": cands[i].excerpt}
			questions[qk] = llmgate.Noul{
				Instructions: fmt.Sprintf(
					"`section` is the title of a long section of a document. `%s.passage` is a "+
						"passage from inside it, beginning with the line `%s.line`. Is that line a "+
						"heading that opens a sub-section a reader could turn to — a note, a topic, a "+
						"statement — rather than a running header, a table's column label, a caption, "+
						"or a sentence fragment?", qk, qk),
				Criteria: &llmgate.NoulCriteria{
					True:  "The line is a sub-section heading and its content follows it",
					False: "The line is page furniture, a label inside a table, or not a heading",
				},
			}
		}
		res, err := b.Judge.Judge(ctx, llmgate.JudgeRequest{State: state, Questions: questions})
		if err != nil {
			return nil, err
		}
		addJudgeUsage(usage, res)
		for qk := range questions {
			p, err := res.Noul(qk)
			if err != nil {
				continue
			}
			var i int
			fmt.Sscanf(qk, "h_%d", &i)
			prob[i] = p
		}
	}
	// Headings found without an index are the weaker source, so the
	// budget is tighter: about one sub-leaf per half-threshold of
	// pages, the most confident kept, then put back in page order.
	// Without this every threshold produced the same 68 leaves per
	// filing — the per-page cap, not the content, was deciding.
	type scored struct {
		n tree.TOCNode
		p float64
	}
	var ss []scored
	for i, c := range cands {
		if prob[i] > th && normalise(c.text) != normalise(leaf.Title) {
			ss = append(ss, scored{tree.TOCNode{Title: c.text, StartPage: c.page}, prob[i]})
		}
	}
	span := leaf.EndPage - leaf.StartPage + 1
	max := span * 2 / over
	if max < splitMinEntries {
		max = splitMinEntries
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].p > ss[j].p })
	if len(ss) > max {
		ss = ss[:max]
	}
	var subs []tree.TOCNode
	seenPage := map[int]bool{}
	for _, x := range ss {
		if seenPage[x.n.StartPage] {
			continue
		}
		seenPage[x.n.StartPage] = true
		subs = append(subs, x.n)
	}
	sortByStart(subs)
	return subs, nil
}

func sortByStart(ns []tree.TOCNode) {
	for i := 1; i < len(ns); i++ {
		for j := i; j > 0 && ns[j].StartPage < ns[j-1].StartPage; j-- {
			ns[j], ns[j-1] = ns[j-1], ns[j]
		}
	}
}

// capSubLeaves keeps at most one sub-leaf per two pages of the parent,
// dropping later entries that share a page with an earlier one first.
func capSubLeaves(subs []tree.TOCNode, leaf *tree.TOCNode) []tree.TOCNode {
	span := leaf.EndPage - leaf.StartPage + 1
	max := int(float64(span) * splitMaxPerPage)
	if max < splitMinEntries {
		max = splitMinEntries
	}
	if len(subs) <= max {
		return subs
	}
	// Prefer one per page: drop same-page repeats, then truncate.
	var out []tree.TOCNode
	lastPage := -1
	for _, s := range subs {
		if s.StartPage == lastPage {
			continue
		}
		out = append(out, s)
		lastPage = s.StartPage
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}
