package retrieval

import (
	"context"
	"strings"
	"unicode"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// MinUnitChars is the least body text a returned unit carries, after its
// own heading is taken off. A shorter unit is a heading ("THE BOEING
// COMPANY", "PART II") or a stub, and it would take a top-k slot from real
// evidence (HAL-3160: 17% of FinanceBench units were under this, 18 of 239
// empty).
const MinUnitChars = 150

// unitExpandChars bounds how much text a heading-only section may gather
// from its descendants. A heading over a compact block becomes that block;
// a cover heading over the whole document is dropped instead of becoming
// the document's first pages.
const unitExpandChars = 6000

// unitExpandPages is the most pages a heading may span and still be stood
// in for by the text beneath it.
const unitExpandPages = 2

// maxRunnerUps caps the runner-up pages and leaves a strategy reports.
const maxRunnerUps = 8

// duplicateShare is the share of a section's word shingles that, when
// already present in the units returned before it, makes it a repeat of
// them.
const duplicateShare = 0.8

const shingleWords = 6

// UnitKind says what a returned unit is.
type UnitKind string

const (
	// UnitPage is one page of the document, as a page-based strategy read it.
	UnitPage UnitKind = "page"
	// UnitSection is one section of the document tree.
	UnitSection UnitKind = "section"
)

// Unit is one piece of evidence a query returns: a page or a section,
// with the text the caller reads.
type Unit struct {
	Kind    UnitKind
	Page    EvidencePage  // Kind == UnitPage
	Section *tree.Section // Kind == UnitSection
	Content string
	// Expanded is true when a heading-only section's text was gathered
	// from the sections beneath it.
	Expanded bool
	// RunnerUp is true when the unit was ranked below the strategy's
	// evidence and fills a slot the evidence left open.
	RunnerUp bool
	// Whole is true when the unit is part of a short document returned
	// entire, past the strategy's own ranking.
	Whole bool
}

// UnitStats counts what assembly left out and filled in.
type UnitStats struct {
	Empty      int  `json:"empty"`      // sections dropped for having no body text
	Duplicate  int  `json:"duplicate"`  // sections dropped as repeats of units already returned
	Expanded   int  `json:"expanded"`   // heading-only sections filled from the text beneath them
	RunnerUps  int  `json:"runner_ups"` // slots filled from below the evidence
	Neighbours int  `json:"neighbours"` // slots filled by a page beside the top evidence page
	Whole      bool `json:"whole_document"`
}

// ContentLoader reads a section's stored text by its content reference.
type ContentLoader func(ctx context.Context, ref string) (string, error)

// AssembleUnits builds the evidence a query returns, best first:
//
//  1. the strategy's evidence pages, as found;
//  2. its selected sections, each with body text: a heading-only section
//     takes the text beneath it when that is compact, and is dropped when
//     it is not; a section whose text the units before it already carry is
//     dropped as a repeat;
//  3. when k is set and slots remain, the pages beside the top evidence
//     page, then the pages and the leaves the strategy ranked just below
//     its evidence, under the same rules.
//
// k <= 0 returns every unit of steps 1 and 2 and fills nothing.
//
// A short document (res.WholeDocument) comes back entire whatever k is: its
// ranked units first, then the rest of it in document order.
func AssembleUnits(ctx context.Context, t *tree.Tree, res *Result, k int, load ContentLoader) ([]Unit, UnitStats) {
	if res == nil {
		return nil, UnitStats{}
	}
	if res.WholeDocument {
		return assembleWhole(ctx, t, res, load)
	}
	a := NewUnitAssembler(t, k, load)
	for _, ep := range res.EvidencePages {
		a.AddPage(ep, false)
	}
	for _, id := range res.SelectedIDs {
		a.AddSection(ctx, id, false)
	}
	if k > 0 {
		for _, ep := range res.NeighbourPages {
			a.AddPage(ep, true)
		}
		for _, ep := range res.RunnerUpPages {
			a.AddPage(ep, true)
		}
		for _, id := range res.RunnerUpIDs {
			a.AddSection(ctx, id, true)
		}
	}
	return a.Units(), a.Stats()
}

// assembleWhole returns a short document entire: what navigation ranked,
// best first, then the rest in document order, with no cap.
func assembleWhole(ctx context.Context, t *tree.Tree, res *Result, load ContentLoader) ([]Unit, UnitStats) {
	a := NewUnitAssembler(t, 0, load)
	for _, ep := range res.EvidencePages {
		a.AddPage(ep, false)
	}
	for _, id := range res.SelectedIDs {
		a.AddSection(ctx, id, false)
	}
	for _, ep := range res.RunnerUpPages {
		a.AddPage(ep, true)
	}
	for _, id := range res.RunnerUpIDs {
		a.AddSection(ctx, id, true)
	}
	ranked := len(a.units)
	for _, ep := range res.DocumentPages {
		a.AddPage(ep, false)
	}
	for _, id := range res.DocumentIDs {
		a.AddSection(ctx, id, false)
	}
	for i := ranked; i < len(a.units); i++ {
		a.units[i].Whole = true
	}
	a.stats.Whole = true
	return a.Units(), a.Stats()
}

// UnitAssembler applies AssembleUnits's rules one unit at a time, for a
// caller that receives units as they are found (a stream).
type UnitAssembler struct {
	t     *tree.Tree
	k     int
	load  ContentLoader
	units []Unit
	stats UnitStats
	seen  map[string]bool
	pages map[string]bool
	ids   map[tree.SectionID]bool
}

// NewUnitAssembler starts an empty assembly capped at k units (k <= 0:
// no cap).
func NewUnitAssembler(t *tree.Tree, k int, load ContentLoader) *UnitAssembler {
	return &UnitAssembler{t: t, k: k, load: load, seen: map[string]bool{}, pages: map[string]bool{}, ids: map[tree.SectionID]bool{}}
}

// Full reports whether the cap is reached.
func (a *UnitAssembler) Full() bool { return a.k > 0 && len(a.units) >= a.k }

// Units are the units accepted so far, in order.
func (a *UnitAssembler) Units() []Unit { return a.units }

// Stats counts what was left out and filled in.
func (a *UnitAssembler) Stats() UnitStats { return a.stats }

// AddPage accepts a page unless the cap is reached, the page is already in,
// or it has no text.
func (a *UnitAssembler) AddPage(ep EvidencePage, runnerUp bool) (Unit, bool) {
	if a.Full() || a.pages[ep.Key()] || len(substantive(ep.Text, "")) == 0 {
		return Unit{}, false
	}
	a.pages[ep.Key()] = true
	u := Unit{Kind: UnitPage, Page: ep, Content: ep.Text, RunnerUp: runnerUp}
	a.units = append(a.units, u)
	addShingles(a.seen, ep.Text)
	switch {
	case ep.NeighbourOf > 0:
		a.stats.Neighbours++
	case runnerUp:
		a.stats.RunnerUps++
	}
	return u, true
}

// AddSection accepts a section with its body text, gathering a compact
// heading's text from beneath it; it refuses one with no body text, a
// repeat of the units already in, or any once the cap is reached.
func (a *UnitAssembler) AddSection(ctx context.Context, id tree.SectionID, runnerUp bool) (Unit, bool) {
	if a.Full() || a.ids[id] || a.t == nil {
		return Unit{}, false
	}
	sec := a.t.FindByID(id)
	if sec == nil {
		return Unit{}, false
	}
	a.ids[id] = true
	content := loadContent(ctx, sec, a.load)
	expanded := false
	if len(substantive(content, sec.Title)) < MinUnitChars {
		gathered, ok := gatherBeneath(ctx, sec, a.load)
		if !ok || len(substantive(gathered, sec.Title)) < MinUnitChars {
			a.stats.Empty++
			return Unit{}, false
		}
		content, expanded = gathered, true
	}
	if repeats(a.seen, content) {
		a.stats.Duplicate++
		return Unit{}, false
	}
	u := Unit{Kind: UnitSection, Section: sec, Content: content, Expanded: expanded, RunnerUp: runnerUp}
	a.units = append(a.units, u)
	addShingles(a.seen, content)
	if expanded {
		a.stats.Expanded++
	}
	if runnerUp {
		a.stats.RunnerUps++
	}
	return u, true
}

func loadContent(ctx context.Context, sec *tree.Section, load ContentLoader) string {
	if sec.ContentRef == "" || load == nil {
		return ""
	}
	s, err := load(ctx, sec.ContentRef)
	if err != nil {
		return ""
	}
	return s
}

// gatherBeneath is a heading's text: the stored text of every section
// beneath it, in document order, each under its own heading. ok is false
// when the heading spans more than unitExpandPages pages or its text would
// exceed unitExpandChars, so a heading over a large part of the document
// is not stood in for by that part's opening pages.
func gatherBeneath(ctx context.Context, sec *tree.Section, load ContentLoader) (string, bool) {
	if len(sec.Children) == 0 {
		return "", false
	}
	// A heading over more than unitExpandPages pages is a part or a cover,
	// not a heading over one block.
	if sec.PageEnd > 0 && sec.PageEnd-sec.PageStart+1 > unitExpandPages {
		return "", false
	}
	estimate := 0
	sec.Walk(func(s *tree.Section) bool {
		if s != sec && s.ContentRef != "" {
			estimate += s.TokenCount * 4
		}
		return estimate <= unitExpandChars
	})
	if estimate > unitExpandChars {
		return "", false
	}
	var b strings.Builder
	over := false
	sec.Walk(func(s *tree.Section) bool {
		if s == sec || over {
			return !over
		}
		text := strings.TrimSpace(loadContent(ctx, s, load))
		if text == "" {
			return true
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if title := strings.TrimSpace(s.Title); title != "" && !strings.HasPrefix(text, title) {
			b.WriteString(title)
			b.WriteString("\n")
		}
		b.WriteString(text)
		if b.Len() > unitExpandChars {
			over = true
		}
		return !over
	})
	if over {
		return "", false
	}
	return b.String(), true
}

// substantive is text with its leading heading and surrounding space taken
// off: what a reader gets beyond the title.
func substantive(text, title string) string {
	s := strings.TrimSpace(text)
	if title = strings.TrimSpace(title); title != "" {
		// EqualFold on the same byte span: lower-casing can change a
		// rune's byte length (U+212A KELVIN SIGN is 3 bytes, "k" is 1), so
		// slicing s by a lower-cased length could panic.
		if len(s) >= len(title) && strings.EqualFold(s[:len(title)], title) {
			s = strings.TrimSpace(s[len(title):])
		}
	}
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })
}

func shingles(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(words) < shingleWords {
		if len(words) == 0 {
			return nil
		}
		return []string{strings.Join(words, " ")}
	}
	out := make([]string, 0, len(words)-shingleWords+1)
	for i := 0; i+shingleWords <= len(words); i++ {
		out = append(out, strings.Join(words[i:i+shingleWords], " "))
	}
	return out
}

func addShingles(seen map[string]bool, text string) {
	for _, s := range shingles(text) {
		seen[s] = true
	}
}

// repeats reports whether the units already returned carry most of text.
func repeats(seen map[string]bool, text string) bool {
	sh := shingles(text)
	if len(sh) == 0 || len(seen) == 0 {
		return false
	}
	hit := 0
	for _, s := range sh {
		if seen[s] {
			hit++
		}
	}
	return float64(hit) >= duplicateShare*float64(len(sh))
}
