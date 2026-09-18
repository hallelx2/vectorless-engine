package ingest

import (
	"regexp"
	"strings"
)

// prefilter.go decides, in microseconds and without a model, whether a
// page is worth asking a model about.
//
// # Why this exists
//
// A Judge's latency is dominated by the tokens it is sent, not by the
// questions asked of them. Detection over a 20-page prefix costs about
// ten request-floors for one request, because the prefix is ~20k tokens
// of state. Most of those pages are cover sheets, legal boilerplate and
// body text that no reader would mistake for a table of contents. Sending
// them costs time and money to learn nothing.
//
// So the rule the whole pipeline is being engineered towards: every
// token sent has to earn its place. Pages with no structural sign of a
// table of contents do not get sent.
//
// # The one rule that keeps this safe
//
// A page is skipped ONLY when it shows zero signal. Not low signal — zero.
// A threshold would be a place for recall to leak out quietly, one
// unusual layout at a time, and the model is much better at the
// ambiguous cases than any heuristic. The heuristic's only job is to
// throw away the pages that are obviously not what we are looking for,
// and to be certain when it does.
//
// # What a table of contents looks like once the parser has had it
//
// Not like a table of contents. The PDF parser flattens layout, so a
// 10-K's TOC arrives as one long run:
//
//	Beginning Page PART I ITEM 1 Business 4 ITEM 1A Risk Factors 10
//	ITEM 1B Unresolved Staff Comments 12 ITEM 2 Properties 12 ...
//
// No line breaks, so "lines ending in a number" — the obvious signal —
// does not exist. What survives flattening is the ENTRY shape: a short
// title followed by a small number, repeated. Prose does not do that
// more than once or twice by accident. That repeated shape is the
// generic signal; ITEM / PART markers are the 10-K-specific booster.

var (
	// A title of a few words followed by a 1–3 digit page number. The
	// lazy quantifier keeps a match from swallowing half the page and
	// counting as one entry.
	reTOCEntry = regexp.MustCompile(`[A-Za-z][A-Za-z ,'&/\-\.]{2,60}?\s+\d{1,3}(?:\s|$)`)

	// SEC form structure — as a TOC ENTRY, not a cross-reference.
	//
	// A 10-K's body says "see Item 7 of this report" on nearly every page,
	// so a bare ITEM marker is no evidence of anything; the first cut of
	// this filter kept 83% of body pages on that rule alone. What a
	// contents page has that a body page does not is the page number
	// sitting right behind the entry: "ITEM 1A Risk Factors 10". So the
	// marker only counts when a small number follows within a title's
	// length, and PART only counts when an ITEM entry follows it.
	reItem = regexp.MustCompile(`(?i)\bITEM\s+\d{1,2}[A-C]?\b[^.;]{0,80}?\s\d{1,3}(?:\s|$)`)
	rePart = regexp.MustCompile(`(?i)\bPART\s+(?:I|II|III|IV)\b\s+ITEM\s+\d`)

	// Words that name the thing itself.
	reTOCWord = regexp.MustCompile(`(?i)\b(?:table\s+of\s+contents|contents|index\s+to|beginning\s+page|page\s+no\.?)\b`)
)

// tocSignal is what the pre-filter measured on one page.
type tocSignal struct {
	Entries  int  // title-then-number shapes, anywhere on the page
	DenseRun int  // the most of those found inside any denseWindow chars
	Items    int  // ITEM markers
	Parts    int  // PART markers
	Keyword  bool // names itself as a contents page
	Chars    int  // page length, for the density reading
}

// Any reports whether the page shows any sign at all of being a table of
// contents. This is the only thing the skip decision reads.
//
// Counting entries is not enough. Financial prose matches the shape
// three times in a paragraph without trying — "Section 4", "a term of
// 5 years", "increased 3 percent" — so a raw count of three is an
// accident, not a list. What prose never does is pack them: a contents
// page puts entry after entry with nothing between, so four of them land
// inside a few hundred characters. That density is the signal; scattered
// matches are not.
//
// Items, parts and the keyword count from one, because a body page rarely
// says "Item 1A" at all, and a page that calls itself a table of contents
// has told us what it is.
func (s tocSignal) Any() bool {
	return s.DenseRun >= denseMin || s.Items >= 1 || s.Parts >= 1 || s.Keyword
}

// denseWindow and denseMin define a "run": denseMin entries starting
// within denseWindow characters of each other. 500 chars fits five
// entries even with long titles ("Management's Discussion and Analysis
// of Financial Condition and Results of Operations 16" is ~90), and the
// prose accident rate — three in ~450 — stays under four.
const (
	denseWindow = 500
	denseMin    = 4
)

// Score orders pages that passed Any, so a two-stage scan can send the
// likeliest first. It is NOT used to skip anything.
func (s tocSignal) Score() int {
	sc := s.Entries + 3*s.Items + 2*s.Parts
	if s.Keyword {
		sc += 10
	}
	return sc
}

// prefilterTOC measures a page. Cheap enough to run on every page of
// every document without thinking about it.
func prefilterTOC(text string) tocSignal {
	t := strings.TrimSpace(text)
	if t == "" {
		return tocSignal{}
	}
	// Bound the work: the shape we want appears early, and a 450k-char
	// "page" (a known parser failure) should not cost a regex pass over
	// all of it.
	if len(t) > 16000 {
		t = t[:16000]
	}
	entries := reTOCEntry.FindAllStringIndex(t, -1)
	return tocSignal{
		Entries:  len(entries),
		DenseRun: densestRun(entries, denseWindow),
		Items:    len(reItem.FindAllStringIndex(t, -1)),
		Parts:    len(rePart.FindAllStringIndex(t, -1)),
		Keyword:  reTOCWord.MatchString(t),
		Chars:    len(text),
	}
}

// densestRun returns the largest number of matches whose start offsets
// fall within window characters of one another. Quadratic in the match
// count, which is tens at most on a real page.
func densestRun(matches [][]int, window int) int {
	best := 0
	for i := range matches {
		n := 0
		for j := i; j < len(matches) && matches[j][0]-matches[i][0] <= window; j++ {
			n++
		}
		if n > best {
			best = n
		}
	}
	return best
}
