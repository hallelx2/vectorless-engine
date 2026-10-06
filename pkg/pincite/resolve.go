package pincite

import (
	"strings"
	"unicode"
)

// FuzzyThreshold is the share of a quote's tokens a window of the page
// must contain for a non-exact match to count. It is the same tolerance
// the dashboard's grounding check uses (≥80% token overlap), so a quote
// the dashboard calls grounded is one this resolver can place.
const FuzzyThreshold = 0.8

// minFuzzyTokens: below this many tokens a fuzzy match is too easy to
// land in the wrong place, so a short quote must match exactly.
const minFuzzyTokens = 4

// Match is a quote located on a page.
type Match struct {
	Page      int       `json:"page"`
	Regions   []Region  `json:"regions"`
	Precision Precision `json:"precision"`
	// Score is 1 for an exact match, else the token overlap that
	// cleared FuzzyThreshold.
	Score float64 `json:"score"`
}

// token is one normalised token of a page and the word it came from.
type token struct {
	text string
	line int
	word int
}

// Tokenize normalises text the way the resolver compares it: lower
// case, typographic quotes, dashes and ligatures folded, and split on
// every run of characters that are neither letters nor digits. "16,048"
// becomes ["16" "048"] on both sides, so figures match without the
// resolver having to understand number formats.
func Tokenize(s string) []string {
	s = fold(s)
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

var folder = strings.NewReplacer(
	"‘", "'", "’", "'", "“", `"`, "”", `"`,
	"–", "-", "—", "-", "−", "-",
	"ﬀ", "ff", "ﬁ", "fi", "ﬂ", "fl", "ﬃ", "ffi", "ﬄ", "ffl",
	" ", " ",
)

func fold(s string) string { return strings.ToLower(folder.Replace(s)) }

func pageTokens(p *Page) []token {
	var out []token
	for li, ln := range p.Lines {
		for wi, w := range ln.Words {
			for _, t := range Tokenize(w.Text) {
				out = append(out, token{text: t, line: li, word: wi})
			}
		}
	}
	return out
}

// Locate finds quote on page p. ok is false when the quote cannot be
// placed: a confidently misplaced highlight is worse than none, so the
// resolver returns nothing rather than its best wrong guess.
func Locate(p *Page, quote string) (Match, bool) {
	if p == nil {
		return Match{}, false
	}
	q := Tokenize(quote)
	if len(q) == 0 {
		return Match{}, false
	}
	toks := pageTokens(p)
	if len(toks) == 0 {
		return Match{}, false
	}
	if i := exactIndex(toks, q); i >= 0 {
		return Match{Page: p.Number, Regions: regions(p, toks[i:i+len(q)]), Precision: PrecisionLine, Score: 1}, true
	}
	if len(q) < minFuzzyTokens || len(toks) < len(q) {
		return Match{}, false
	}
	start, end, score := bestWindow(toks, q)
	if score < FuzzyThreshold {
		return Match{}, false
	}
	return Match{Page: p.Number, Regions: regions(p, toks[start:end]), Precision: PrecisionLine, Score: score}, true
}

// Resolve locates quote on the first of pages that holds it exactly,
// else on the page with the best fuzzy match. With no match it returns
// a page-level citation on the first candidate page that exists in the
// layout, so the reader is still taken to the right page.
func Resolve(l *Layout, quote string, pages []int) Match {
	var best Match
	found := false
	for _, n := range pages {
		m, ok := Locate(l.Page(n), quote)
		if !ok {
			continue
		}
		if m.Score == 1 {
			return m
		}
		if !found || m.Score > best.Score {
			best, found = m, true
		}
	}
	if found {
		return best
	}
	for _, n := range pages {
		if l.Page(n) != nil {
			return Match{Page: n, Precision: PrecisionPage}
		}
	}
	if len(pages) > 0 {
		return Match{Page: pages[0], Precision: PrecisionPage}
	}
	return Match{Precision: PrecisionSection}
}

func exactIndex(toks []token, q []string) int {
outer:
	for i := 0; i+len(q) <= len(toks); i++ {
		for j := range q {
			if toks[i+j].text != q[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

// maxStretch is how much longer than the quote the matched passage may
// be. A model's quote often drops a parenthetical ("segment profit/loss
// (business segment operating income) was updated" quoted without the
// brackets), so the passage it came from is longer than the quote.
const maxStretch = 0.5

// bestWindow finds the passage covering the most of the quote's tokens
// (as a multiset), letting the window grow up to maxStretch beyond the
// quote's length; among windows covering equally much, the shortest
// wins. The window is then trimmed at both ends while an end token is
// one the quote does not need — absent from it, or already covered
// inside the window — so the highlight never starts or ends on a word
// the quote does not account for.
func bestWindow(toks []token, q []string) (start, end int, score float64) {
	need := map[string]int{}
	for _, t := range q {
		need[t]++
	}
	m := len(q)
	maxW := min(len(toks), m+int(float64(m)*maxStretch))
	bestS, bestE, bestO := 0, 0, -1
	for w := m; w <= maxW; w++ {
		have := map[string]int{}
		overlap := 0
		add := func(t string, d int) {
			before := min(have[t], need[t])
			have[t] += d
			overlap += min(have[t], need[t]) - before
		}
		for i := 0; i < w; i++ {
			add(toks[i].text, 1)
		}
		if overlap > bestO {
			bestS, bestE, bestO = 0, w, overlap
		}
		for i := 1; i+w <= len(toks); i++ {
			add(toks[i-1].text, -1)
			add(toks[i+w-1].text, 1)
			if overlap > bestO {
				bestS, bestE, bestO = i, i+w, overlap
			}
		}
		if bestO == m {
			break // a shorter window already covers the whole quote
		}
	}
	start, end = bestS, bestE
	have := map[string]int{}
	for _, t := range toks[start:end] {
		have[t.text]++
	}
	surplus := func(t string) bool { return have[t] > need[t] }
	for start < end && surplus(toks[start].text) {
		have[toks[start].text]--
		start++
	}
	for end > start && surplus(toks[end-1].text) {
		have[toks[end-1].text]--
		end--
	}
	return start, end, float64(bestO) / float64(m)
}

// regions turns a run of matched tokens into one rectangle per line,
// spanning exactly the words the run covers on that line.
func regions(p *Page, run []token) []Region {
	var out []Region
	curLine := -1
	var x0, x1 float64
	flush := func() {
		if curLine < 0 {
			return
		}
		ln := p.Lines[curLine]
		out = append(out, Region{Left: x0, Top: ln.Top, Width: x1 - x0, Height: ln.Height})
	}
	for _, t := range run {
		w := p.Lines[t.line].Words[t.word]
		if t.line != curLine {
			flush()
			curLine, x0, x1 = t.line, w.X0, w.X1
			continue
		}
		x0 = min(x0, w.X0)
		x1 = max(x1, w.X1)
	}
	flush()
	return out
}
