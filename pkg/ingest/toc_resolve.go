package ingest

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// toc_resolve.go locates where each section actually begins, by searching
// for its title and letting a Judge confirm the hit.
//
// # The problem this replaces
//
// A table of contents says "Item 1A. Risk Factors ... 20". That 20 is the
// document's own printed page number; the PDF has a cover and a contents
// page in front of printed page 1, so the section is at PDF page 22 or
// so. Extraction asks a chat model to resolve this by reading the body —
// but the body it is shown is capped at ~48k characters, five pages of a
// five-hundred-page filing, so for anything deeper it guesses from the
// printed number. Verification then looks at the wrong page and, rightly,
// says no. Every leaf past page five loses its page. Every long filing.
//
// # Select, don't generate
//
// Finding a title is a lookup with a small answer set, so the candidates
// come from code: scan every page's head for the title. That usually
// yields one to three pages — the real opening, the contents page itself,
// and now and then a body page that mentions the section in passing.
//
// Telling those apart is the judgement: is the title a HEADING on this
// page, or merely mentioned on it? A Judge answers that for every (leaf,
// candidate) pair in one request. Code takes the best answer above
// threshold.
//
// The question is deliberately "on this page", not "at the very start of
// it". Filings pack several short sections onto one page — Items 1B, 2,
// 3 and 4 of a 10-K routinely share a page — and only the first of them
// opens the page. The others still start there. Asking for "the very
// start" threw away half of ADOBE's leaves for being second on their
// page, which is a fact about layout, not about where the section is.
//
// The contents page contains every title as a heading-shaped entry and
// would win every question. It is excluded in code: detection already
// knows which page it is, and certainty beats asking.
//
// This is verification with search rather than verification of a guess.
// It makes the extraction body window irrelevant to page accuracy, and it
// works for any document, not only ones with a numbered contents page.

// Window around a located heading that is shown to the Judge: its own
// line up to excerptBefore characters back, and enough after to see a
// section actually begin. This replaces sending the page
// head, which missed every section that was third or later on a shared
// page — Items 3 and 4 of a 10-K after 1B and 2, 9B and 9C after 9 and
// 9A. Go finds the exact spot; the model only has to confirm it.
const (
	excerptBefore = 150
	excerptAfter  = 450

	// headChars is retained for the test that proves a deep hit lies
	// beyond any head-sized window.
	headChars = 1200

	// scanChars bounds the per-page search so a 450k-char "page" (a known
	// parser failure, HAL-1365) does not cost a regex pass over all of it.
	scanChars = 60000
)

// maxCandidatesPerLeaf bounds the Judge batch. A title that appears at
// the head of more than this many pages is a running header, not a
// section, and no amount of asking will resolve it.
const maxCandidatesPerLeaf = 4

var (
	reNonWord = regexp.MustCompile(`[^a-z0-9]+`)
)

// normalise folds a title or page head into a form where "Item 1A. Risk
// Factors" and "ITEM 1A — RISK FACTORS" compare equal.
func normalise(s string) string {
	s = strings.ToLower(s)
	s = reNonWord.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// pageHit is one place a title was found: the page, and where on it.
type pageHit struct {
	page   int
	offset int // byte offset of the match in the page text
}

// titleRegexp builds a matcher for a title that tolerates the punctuation
// and spacing differences between a contents entry and a body heading:
// "Item 1A. Risk Factors" must find "ITEM 1A — RISK FACTORS".
func titleRegexp(title string) *regexp.Regexp {
	words := contentWords(title)
	if len(words) == 0 {
		return nil
	}
	// Long titles are often wrapped or abbreviated on their own page;
	// match on the first several words.
	if len(words) > 6 {
		words = words[:6]
	}
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = regexp.QuoteMeta(w)
	}
	// Between two content words, any punctuation and any run of
	// stopwords: "Exhibits, Financial Statement Schedules" in the
	// contents is "Exhibits and Financial Statement Schedules" on its
	// page (BOEING_2022_10K, Item 15).
	sep := `[^a-z0-9]+(?:(?:` + strings.Join(stopwords, "|") + `)[^a-z0-9]+)*`
	return regexp.MustCompile(`(?i)` + strings.Join(parts, sep))
}

// stopwords are dropped from a title before matching and tolerated
// between its words on the page.
var stopwords = []string{"and", "of", "the", "to", "for", "a", "an", "in", "on", "or"}

// contentWords is the normalised title minus stopwords.
func contentWords(title string) []string {
	var out []string
	for _, w := range strings.Fields(normalise(title)) {
		stop := false
		for _, sw := range stopwords {
			if w == sw {
				stop = true
				break
			}
		}
		if !stop {
			out = append(out, w)
		}
	}
	return out
}

// findCandidatePages returns where the title appears as a HEADING on any
// page, in page order. A mention inside a sentence is a cross-reference,
// not a section, and is not a candidate.
//
// Two tiers. A hit that opens its line is a heading. Only when a title
// has NO such hit anywhere does the second tier apply: a hit that is set
// like a heading — every content word capitalised in the text itself —
// with at most a few words before it on the line. That is how a
// financial statement is titled: "Amcor plc and Subsidiaries Consolidated
// Balance Sheet (in millions)". A lowercase "see the consolidated balance
// sheet" is never a candidate.
func findCandidatePages(title string, pages []PageText) []pageHit {
	if len(normalise(title)) < 8 {
		return nil
	}
	re := titleRegexp(title)
	if re == nil {
		return nil
	}

	lineStart, typographic := scanPages(re, pages)
	// A numbered section is often re-worded on its own page: "Item 5.
	// Market For Registrant's Common Equity, ..." in the contents is
	// "Item 5. - Market for Registrant's Equity, ..." on page 21 of
	// AMCOR_2020_10K. The label, number and first word are distinctive
	// enough to add as candidates; the Judge sees the page and decides.
	if loose := looseNumberedRegexp(title); loose != nil {
		ls, ty := scanPages(loose, pages)
		lineStart = unionHits(lineStart, ls)
		typographic = unionHits(typographic, ty)
	}
	out := lineStart
	if len(out) == 0 {
		out = typographic
	}
	sort.Slice(out, func(i, j int) bool { return out[i].page < out[j].page })
	return out
}

// unionHits merges two hit lists, one hit per page, the first list's
// offset winning.
func unionHits(a, b []pageHit) []pageHit {
	out := append([]pageHit(nil), a...)
	for _, h := range b {
		if !hasPage(out, h.page) {
			out = append(out, h)
		}
	}
	return out
}

// numberedLabels open the titles that looseNumberedRegexp applies to.
var numberedLabels = map[string]bool{"item": true, "part": true, "note": true, "section": true, "chapter": true, "article": true}

// looseNumberedRegexp matches a numbered title by its label, number and
// first content word only. Nil for titles that are not numbered, or too
// short for the loose form to be any looser.
func looseNumberedRegexp(title string) *regexp.Regexp {
	words := contentWords(title)
	if len(words) < 4 || !numberedLabels[words[0]] {
		return nil
	}
	if _, err := strconv.Atoi(strings.TrimRight(words[1], "abcdefghijklmnopqrstuvwxyz")); err != nil {
		return nil
	}
	sep := `[^a-z0-9]+(?:(?:` + strings.Join(stopwords, "|") + `)[^a-z0-9]+)*`
	return regexp.MustCompile(`(?i)` + regexp.QuoteMeta(words[0]) + sep + regexp.QuoteMeta(words[1]) + sep + regexp.QuoteMeta(words[2]) + `\b`)
}

// scanPages finds each page's first line-opening hit and, failing that,
// its first typographic-heading hit.
func scanPages(re *regexp.Regexp, pages []PageText) (lineStart, typographic []pageHit) {
	for _, p := range pages {
		if p.PageNumber <= 0 {
			continue
		}
		text := p.Text
		if len(text) > scanChars {
			text = text[:scanChars]
		}
		foundLS, foundTypo := false, false
		for _, m := range re.FindAllStringIndex(text, -1) {
			switch {
			case !foundLS && isLineStart(text, m[0]):
				lineStart = append(lineStart, pageHit{page: p.PageNumber, offset: m[0]})
				foundLS = true
			case !foundTypo && isSetLikeAHeading(text, m[0], m[1]):
				typographic = append(typographic, pageHit{page: p.PageNumber, offset: m[0]})
				foundTypo = true
			}
			if foundLS {
				break
			}
		}
	}
	return lineStart, typographic
}

// maxHeadingPrefixWords is how many words may precede a typographic
// heading on its line — a company name, not a sentence.
const maxHeadingPrefixWords = 5

// isSetLikeAHeading reports whether the match at [lo,hi) is capitalised
// the way a heading is and sits near the start of its line.
func isSetLikeAHeading(text string, lo, hi int) bool {
	ls := lo
	for ls > 0 && text[ls-1] != '\n' {
		ls--
	}
	prefix := strings.Fields(text[ls:lo])
	if len(prefix) > maxHeadingPrefixWords {
		return false
	}
	for _, w := range prefix {
		// A sentence before the match, not a label: "see the", "in our".
		if strings.HasSuffix(w, ".") || strings.HasSuffix(w, ",") || strings.HasSuffix(w, ";") {
			return false
		}
	}
	match := text[lo:hi]
	for _, w := range strings.Fields(match) {
		r := []rune(w)[0]
		if unicode.IsLetter(r) && !unicode.IsUpper(r) && !isStopword(strings.ToLower(w)) {
			return false
		}
	}
	return true
}

func isStopword(w string) bool {
	w = strings.Trim(w, ".,;:()")
	for _, sw := range stopwords {
		if w == sw {
			return true
		}
	}
	return false
}

// rePartLabel is the one thing allowed before a heading on its line: the
// parser routinely joins a part label to the item that opens it —
// "PART I ITEM 1. BUSINESS" — and every first-in-part section of a 10-K
// was lost to that until this was measured on ADOBE_2022_10K.
var rePartLabel = regexp.MustCompile(`(?i)^part\s+[ivxlc\d]+\s*[.:\-—–]?$`)

// isLineStart reports whether the match at off opens its line — what a
// heading looks like once the parser has had it, and what a mid-sentence
// cross-reference does not. A part label alone before it still counts.
func isLineStart(text string, off int) bool {
	lo := off
	for lo > 0 && text[lo-1] != '\n' {
		lo--
	}
	// The parser marks a heading it folded into a neighbour's body as
	// **bold**; a markdown source may use #. Neither is content.
	prefix := strings.TrimSpace(strings.Trim(text[lo:off], " \t*_#"))
	return prefix == "" || rePartLabel.MatchString(prefix)
}

// excerptAround cuts the window the Judge is shown: from the start of
// the hit's own line, since a heading opens its line and a typographic
// hit's label prefix is on the same line, through enough text after it
// to see the section begin. Nothing from earlier lines — on a page that
// opens a section beneath its contents list (BOEING_2022_10K, Item 1)
// the lines above are the list, and showing them made the Judge read the
// heading as one more entry.
func excerptAround(text string, off int) string {
	lo := off
	for lo > 0 && text[lo-1] != '\n' {
		lo--
	}
	if off-lo > excerptBefore {
		lo = off - excerptBefore
	}
	hi := off + excerptAfter
	if hi > len(text) {
		hi = len(text)
	}
	return text[lo:hi]
}

// resolvedLeaf is one leaf and the pages that might open it.
type resolvedLeaf struct {
	key        string
	title      string
	claimed    int       // what extraction said; 0 if it said nothing
	candidates []pageHit // where the title was actually found
}

// collectResolveClaims walks the tree and gathers every leaf with a
// title, whether or not extraction managed to give it a page.
//
// That last part is the difference from collectLeafClaims: a leaf with no
// claimed page is exactly the one that needs resolving, not the one to
// skip.
func collectResolveClaims(nodes []tree.TOCNode, pages []PageText, exclude []int) []resolvedLeaf {
	var out []resolvedLeaf
	seen := map[string]int{}
	skip := map[int]bool{}
	for _, pg := range exclude {
		skip[pg] = true
	}

	var walk func(ns []tree.TOCNode)
	walk = func(ns []tree.TOCNode) {
		for _, n := range ns {
			if len(n.Nodes) > 0 {
				walk(n.Nodes)
				continue
			}
			if strings.TrimSpace(n.Title) == "" {
				continue
			}
			key := "r_" + slugForKey(n.Title)
			if seen[key] > 0 {
				key = fmt.Sprintf("%s_%d", key, seen[key])
			}
			seen[key]++

			var cands []pageHit
			hits := findCandidatePages(n.Title, pages)
			for _, h := range hits {
				if !skip[h.page] {
					cands = append(cands, h)
				}
			}
			if len(cands) == 0 && len(hits) > 0 && n.StartPage == 0 {
				// Every hit is on a contents page. Usually that means the
				// title only appears in the list — but a 10-K whose Item 1
				// starts on the same page as its contents (BOEING_2022_10K)
				// has nowhere else to be found. Exclusion is a preference
				// when it would otherwise leave nothing; the Judge is told
				// to say no to a list entry.
				cands = hits
			}
			if n.StartPage > 0 && !skip[n.StartPage] && !hasPage(cands, n.StartPage) {
				// Extraction's guess is a candidate too — shown from its
				// head, since we found no title on it to anchor a window.
				cands = append(cands, pageHit{page: n.StartPage, offset: 0})
				sort.Slice(cands, func(i, j int) bool { return cands[i].page < cands[j].page })
			}
			if len(cands) > maxCandidatesPerLeaf {
				// A running header, not a section. Keep the claimed page
				// if there is one; otherwise this leaf cannot be placed.
				if n.StartPage > 0 {
					cands = []pageHit{{page: n.StartPage}}
				} else {
					cands = nil
				}
			}
			out = append(out, resolvedLeaf{key: key, title: n.Title, claimed: n.StartPage, candidates: cands})
		}
	}
	walk(nodes)
	return out
}

func hasPage(hs []pageHit, page int) bool {
	for _, h := range hs {
		if h.page == page {
			return true
		}
	}
	return false
}

// resolvePagesJudge asks, for every (leaf, candidate) pair, whether the
// title begins that page, and returns the best page per leaf.
//
// One request per budget-batch, all leaves together. Returns handled=false
// on failure so the caller falls back to plain verification.
func (b *TOCBuilder) resolvePagesJudge(ctx context.Context, nodes []tree.TOCNode, pages []PageText, exclude []int, usage *Usage) (map[string]int, bool) {
	out, handled, err := b.resolvePagesJudgeErr(ctx, nodes, pages, exclude, usage)
	if err != nil {
		log.Printf("toc: judge page resolution failed, falling back: %v", err)
	}
	return out, handled
}

func (b *TOCBuilder) resolvePagesJudgeErr(ctx context.Context, nodes []tree.TOCNode, pages []PageText, exclude []int, usage *Usage) (map[string]int, bool, error) {
	if b.Judge == nil {
		return nil, false, nil
	}
	byPage := make(map[int]string, len(pages))
	for _, p := range pages {
		byPage[p.PageNumber] = p.Text
	}

	claims := collectResolveClaims(nodes, pages, exclude)
	if len(claims) == 0 {
		return map[string]int{}, true, nil
	}

	type probe struct {
		leafKey string
		page    int
		offset  int
	}
	var probes []probe
	for _, c := range claims {
		for _, h := range c.candidates {
			probes = append(probes, probe{c.leafKey(), h.page, h.offset})
		}
	}
	if len(probes) == 0 {
		return map[string]int{}, true, nil
	}

	titles := make(map[string]string, len(claims))
	for _, c := range claims {
		titles[c.leafKey()] = c.title
	}

	// best[leaf] = (page, probability)
	best := map[string]struct {
		page int
		p    float64
	}{}

	for start := 0; start < len(probes); {
		state := map[string]any{}
		questions := map[string]llmgate.Question{}
		used := 0

		for ; start < len(probes); start++ {
			pr := probes[start]
			excerpt := excerptAround(byPage[pr.page], pr.offset)
			cost := estimateTokens(excerpt) + 80
			if used > 0 && used+cost > judgeBudget {
				break
			}
			qk := fmt.Sprintf("%s_p%d", pr.leafKey, pr.page)
			state[qk] = map[string]any{"title": titles[pr.leafKey], "excerpt": excerpt}
			questions[qk] = llmgate.Noul{
				Instructions: fmt.Sprintf(
					"`%s.excerpt` is a passage from one page of a document. Does the section "+
						"titled `%s.title` begin in this passage — does its heading appear here, "+
						"opening the section? Match the title loosely; ignore spacing, "+
						"capitalisation and punctuation. Answer no if the title appears only as a "+
						"cross-reference such as \"see Item 1A\", or as an entry in a list of contents.",
					qk, qk),
				Criteria: &llmgate.NoulCriteria{
					True:  "The passage contains this section's heading and the section starts there",
					False: "The heading is absent, or the title is only mentioned in passing",
				},
			}
			used += cost
		}
		if len(questions) == 0 {
			continue
		}

		res, err := b.Judge.Judge(ctx, llmgate.JudgeRequest{State: state, Questions: questions})
		if err != nil {
			return nil, false, err
		}
		addJudgeUsage(usage, res)

		for qk := range questions {
			p, err := res.Noul(qk)
			if err != nil {
				continue
			}
			leafKey, page := splitProbeKey(qk)
			if cur, ok := best[leafKey]; !ok || p > cur.p {
				best[leafKey] = struct {
					page int
					p    float64
				}{page, p}
			}
		}
	}

	th := b.judgeThreshold()
	resolved := make(map[string]int, len(best))
	for k, v := range best {
		if v.p > th {
			resolved[k] = v.page
		} else {
			resolved[k] = 0
		}
	}
	return resolved, true, nil
}

func (c resolvedLeaf) leafKey() string { return c.key }

// splitProbeKey undoes the "<leaf>_p<page>" join.
func splitProbeKey(qk string) (string, int) {
	i := strings.LastIndex(qk, "_p")
	if i < 0 {
		return qk, 0
	}
	var page int
	fmt.Sscanf(qk[i+2:], "%d", &page)
	return qk[:i], page
}

// applyResolvedPages writes the Judge's chosen page onto every leaf it
// answered for. A leaf it did not answer for is left alone, for the same
// reason applyJudgeVerdicts leaves absent verdicts alone: not asked is
// not no.
func applyResolvedPages(nodes []tree.TOCNode, resolved map[string]int) {
	if len(resolved) == 0 {
		return
	}
	seen := map[string]int{}
	var walk func(ns []tree.TOCNode)
	walk = func(ns []tree.TOCNode) {
		for i := range ns {
			if len(ns[i].Nodes) > 0 {
				walk(ns[i].Nodes)
				continue
			}
			if strings.TrimSpace(ns[i].Title) == "" {
				continue
			}
			key := "r_" + slugForKey(ns[i].Title)
			if seen[key] > 0 {
				key = fmt.Sprintf("%s_%d", key, seen[key])
			}
			seen[key]++
			if pg, ok := resolved[key]; ok {
				ns[i].StartPage = pg
			}
		}
	}
	walk(nodes)
}
