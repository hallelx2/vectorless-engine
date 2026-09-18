package ingest

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TOC extraction on a Judge — select, don't generate (HAL-1370).
//
// The generative extractor sends the contents pages plus 48k characters
// of body to a chat model and asks it to write the tree back as JSON.
// That call took 103–840 seconds per FinanceBench filing and was the
// entire wall clock of the TOC stage once detection and page resolution
// were on a Judge. It is asking a generative model to do a lookup: the
// contents page already lists every title, in order, with its printed
// page number.
//
// So: Go parses the contents pages into candidate entries, structure
// comes from the numbering, and the Judge answers one Noul per entry —
// is this a section a reader could turn to, or noise? — in one request.
// Pages are not taken from the printed numbers; the resolver finds
// them, and the printed numbers only calibrate an offset for whatever
// the resolver could not place.

// contentsEntry is one candidate line of a table of contents.
type contentsEntry struct {
	Title     string
	Printed   int  // page number as printed in the contents; 0 if none
	Depth     int  // 1 = top level
	Container bool // a PART header: kept without asking, carries no page
}

// minContentsEntries is how many confirmed entries make a contents page
// worth trusting over the generative extractor.
const minContentsEntries = 3

// contentsStateMaxChars bounds the contents text shared with every
// question. Two 10-K contents pages are ~3k characters; this is a
// ceiling for the pathological case, well under the shared-state limit.
const contentsStateMaxChars = 12_000

var (
	// rePartHeader: "PART I", "Part II.", "**Part III**", alone on its
	// line or closing a title line ("... Table of Contents Part I").
	rePartHeader = regexp.MustCompile(`(?i)(?:^|\s)(part\s+(?:[ivxlc]+|\d+))\s*[.:\-—–]?\s*$`)
	// reEntryLabel opens a numbered entry: Item 1A., Note 12, Section 3.
	reEntryLabel = regexp.MustCompile(`(?i)^(?:item|note|section|chapter|article|appendix|schedule|exhibit)$`)
	// reDotted is a dotted heading number: 1, 1.2, 1.2.3, optionally with
	// a trailing dot.
	reDotted = regexp.MustCompile(`^\d+(?:\.\d+)*\.?$`)
	// rePageNum is a printed page number token.
	rePageNum = regexp.MustCompile(`^\d{1,3}$`)
	// reLeaders strips dot leaders and trailing punctuation from a title.
	reLeaders = regexp.MustCompile(`[.\s·…_\-]+$`)
	// reNoise: lines that are the table's own furniture, not entries.
	reNoise = regexp.MustCompile(`(?i)^(?:page(?:\s*no\.?)?|table\s+of\s+contents|contents|index)$`)
)

// parseContentsEntries turns the text of the contents pages into
// candidate entries, in reading order.
//
// The parser runs entries together on one line — "Item 1. Business 3
// Item 1A. Risk Factors 20 …" — so an entry closes at a 1–3 digit page
// number that is followed by the end of the line or by something that
// opens an entry: a label like Item, a dotted number, or a capitalised
// word. "Item 6 [Reserved] 35" survives because "[Reserved]" opens
// nothing; "Form 10-K Summary 96" because "10-K" is not a page number.
// A trailing "2 Table of Contents" footer has no page after it and is
// dropped.
func parseContentsEntries(text string) []contentsEntry {
	var out []contentsEntry
	inPart := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.ReplaceAll(raw, "**", " "))
		if line == "" {
			continue
		}
		if m := rePartHeader.FindStringSubmatch(line); m != nil {
			// A part header, possibly at the end of a title line. Only
			// the part becomes an entry; the rest of the line is the
			// table's own title.
			out = append(out, contentsEntry{Title: strings.ToUpper(strings.Join(strings.Fields(m[1]), " ")), Depth: 1, Container: true})
			inPart = true
			continue
		}
		toks := strings.Fields(line)
		var cur []string
		flush := func(page int) {
			title := cleanContentsTitle(strings.Join(cur, " "))
			cur = nil
			if title == "" || reNoise.MatchString(title) || !hasLetter(title) || len(title) < 3 {
				return
			}
			out = append(out, contentsEntry{Title: title, Printed: page, Depth: entryDepth(title, inPart)})
		}
		for i := 0; i < len(toks); i++ {
			t := toks[i]
			if rePageNum.MatchString(t) && len(cur) > 0 && (i+1 == len(toks) || opensEntry(toks[i+1])) {
				n, _ := strconv.Atoi(t)
				flush(n)
				continue
			}
			cur = append(cur, t)
		}
		// Whatever is left had no page number: a wrapped title fragment
		// or a footer. Neither is an entry on its own.
	}
	return out
}

// opensEntry reports whether a token can begin a new contents entry.
func opensEntry(tok string) bool {
	if reEntryLabel.MatchString(tok) || reDotted.MatchString(tok) {
		return true
	}
	r := []rune(tok)[0]
	return unicode.IsUpper(r)
}

// entryDepth derives the outline depth from the title's own numbering.
// "Item N" sits under a part when one has been seen; "1.2.3" is depth 3
// (+1 under a part); anything else is a sibling at the current level.
func entryDepth(title string, inPart bool) int {
	base := 1
	if inPart {
		base = 2
	}
	f := strings.Fields(title)
	if len(f) == 0 {
		return base
	}
	if reEntryLabel.MatchString(f[0]) {
		return base
	}
	if reDotted.MatchString(f[0]) {
		return base + strings.Count(strings.TrimSuffix(f[0], "."), ".")
	}
	return base
}

func cleanContentsTitle(s string) string {
	s = reLeaders.ReplaceAllString(strings.TrimSpace(s), "")
	return strings.Join(strings.Fields(s), " ")
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// confirmEntriesJudge asks one Noul per non-container entry: is this a
// section of the document, or furniture? Returns which entries to keep.
func (b *TOCBuilder) confirmEntriesJudge(ctx context.Context, entries []contentsEntry, contents string, usage *Usage) ([]bool, error) {
	keep := make([]bool, len(entries))
	if len(contents) > contentsStateMaxChars {
		contents = contents[:contentsStateMaxChars]
	}
	th := b.judgeThreshold()

	// Batch so that shared contents + per-entry state stays under the
	// shared-state ceiling. Entries are ~30 tokens each.
	const perEntry = 40
	perBatch := (judgeBudget - estimateTokens(contents) - 200) / perEntry
	if perBatch < 20 {
		perBatch = 20
	}
	for start := 0; start < len(entries); start += perBatch {
		end := start + perBatch
		if end > len(entries) {
			end = len(entries)
		}
		state := map[string]any{"contents": contents}
		questions := map[string]llmgate.Question{}
		for i := start; i < end; i++ {
			e := entries[i]
			if e.Container {
				keep[i] = true
				continue
			}
			qk := fmt.Sprintf("e_%d", i)
			state[qk] = map[string]any{"title": e.Title, "printed_page": e.Printed}
			questions[qk] = llmgate.Noul{
				Instructions: fmt.Sprintf(
					"`contents` is the text of a document's table of contents. Is `%s.title` "+
						"(listed at page `%s.printed_page`) one of the document's sections — a heading "+
						"a reader could turn to — rather than the table's own title, a running header "+
						"or footer, a column label such as \"Page\", or a fragment cut from a longer "+
						"title?", qk, qk),
				Criteria: &llmgate.NoulCriteria{
					True:  "A real section or chapter of the document, as listed",
					False: "Furniture of the page, or a broken fragment of a title",
				},
			}
		}
		if len(questions) == 0 {
			continue
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
			i, _ := strconv.Atoi(strings.TrimPrefix(qk, "e_"))
			keep[i] = p > th
		}
	}
	return keep, nil
}

// extractFromTOCPagesJudge builds the tree from the contents pages on a
// Judge alone. handled=false with a nil error means the contents did not
// parse into enough entries to trust — the generative extractor should
// run. An error means the Judge failed; the caller decides.
//
// The returned printed map carries each leaf's printed page number,
// keyed the way the resolver keys leaves, for calibrateFromPrinted.
func (b *TOCBuilder) extractFromTOCPagesJudge(ctx context.Context, pages []PageText, tocPages []int, usage *Usage) (nodes []tree.TOCNode, printed map[string]int, handled bool, err error) {
	if b.Judge == nil || len(tocPages) == 0 {
		return nil, nil, false, nil
	}
	contents := joinTOCPagesText(pages, tocPages)
	entries := parseContentsEntries(contents)
	if countRealEntries(entries) < minContentsEntries {
		return nil, nil, false, nil
	}
	keep, err := b.confirmEntriesJudge(ctx, entries, contents, usage)
	if err != nil {
		return nil, nil, false, err
	}
	var kept []contentsEntry
	for i, e := range entries {
		if keep[i] {
			kept = append(kept, e)
		}
	}
	if countRealEntries(kept) < minContentsEntries {
		return nil, nil, false, nil
	}

	// Drop a container that ended up with nothing under it (a "Part V"
	// the parser saw in a footer, say).
	kept = dropEmptyContainers(kept)

	flat := make([]tocNodePayload, 0, len(kept))
	counters := map[int]int{}
	printed = map[string]int{}
	seen := map[string]int{}
	for _, e := range kept {
		d := e.Depth
		if d < 1 {
			d = 1
		}
		counters[d]++
		for k := range counters {
			if k > d {
				delete(counters, k)
			}
		}
		parts := make([]string, d)
		for i := 1; i <= d; i++ {
			c := counters[i]
			if c == 0 {
				c = 1
				counters[i] = 1
			}
			parts[i-1] = strconv.Itoa(c)
		}
		flat = append(flat, tocNodePayload{Structure: strings.Join(parts, "."), Title: e.Title})
		if !e.Container && e.Printed > 0 {
			key := "r_" + slugForKey(e.Title)
			if seen[key] > 0 {
				key = fmt.Sprintf("%s_%d", key, seen[key])
			}
			seen[key]++
			printed[key] = e.Printed
		}
	}
	return assembleHierarchy(flat), printed, true, nil
}

func countRealEntries(es []contentsEntry) int {
	n := 0
	for _, e := range es {
		if !e.Container {
			n++
		}
	}
	return n
}

func dropEmptyContainers(es []contentsEntry) []contentsEntry {
	var out []contentsEntry
	for i, e := range es {
		if e.Container {
			if i+1 >= len(es) || es[i+1].Container {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// calibrateFromPrinted gives a page to every leaf the resolver could
// not place, from its printed page number and the offset the resolved
// leaves agree on. A 10-K's printed page N is physical page N+k for a
// constant k (the cover and contents come before printed page 1); the
// median of (physical − printed) over the resolved leaves is k. Leaves
// keyed as the resolver keys them. Returns how many were placed.
func calibrateFromPrinted(nodes []tree.TOCNode, printed map[string]int, lastPage int) int {
	if len(printed) == 0 {
		return 0
	}
	var offsets []int
	var unplaced []*tree.TOCNode
	seen := map[string]int{}
	var walk func(ns []tree.TOCNode)
	walk = func(ns []tree.TOCNode) {
		for i := range ns {
			n := &ns[i]
			if len(n.Nodes) > 0 {
				walk(n.Nodes)
				continue
			}
			key := "r_" + slugForKey(n.Title)
			if seen[key] > 0 {
				key = fmt.Sprintf("%s_%d", key, seen[key])
			}
			seen[key]++
			pp, ok := printed[key]
			if !ok || pp <= 0 {
				continue
			}
			if n.StartPage > 0 {
				offsets = append(offsets, n.StartPage-pp)
			} else {
				unplaced = append(unplaced, n)
			}
		}
	}
	walk(nodes)
	if len(offsets) < 2 || len(unplaced) == 0 {
		return 0
	}
	sort.Ints(offsets)
	k := offsets[len(offsets)/2]
	placed := 0
	seen = map[string]int{}
	// Second pass to read the printed page of each unplaced leaf again
	// (keys were consumed in order above; recompute the same way).
	var walk2 func(ns []tree.TOCNode)
	walk2 = func(ns []tree.TOCNode) {
		for i := range ns {
			n := &ns[i]
			if len(n.Nodes) > 0 {
				walk2(n.Nodes)
				continue
			}
			key := "r_" + slugForKey(n.Title)
			if seen[key] > 0 {
				key = fmt.Sprintf("%s_%d", key, seen[key])
			}
			seen[key]++
			if n.StartPage != 0 {
				continue
			}
			pp, ok := printed[key]
			if !ok || pp <= 0 {
				continue
			}
			p := pp + k
			if p >= 1 && (lastPage == 0 || p <= lastPage) {
				n.StartPage = p
				placed++
			}
		}
	}
	walk2(nodes)
	return placed
}
