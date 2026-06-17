package tree

import (
	"math"
	"strings"
)

// BuildHeadingPaths reconciles the parser's section tree with the
// LLM-built TOC tree into one canonical lookup: section ID → logical
// heading path (the chain of TOC titles from the document root down to
// the most specific TOC node that covers the section's pages).
//
// This is the bridge HAL-109 introduces. Ingestion builds two
// independent structures:
//
//   - the parser's Section tree, which carries content, summaries, and
//     candidate questions, and whose IDs every citation resolves to; its
//     Title fields are whatever the parser recovered (often empty or a
//     non-semantic chunk label for content leaves), and
//   - the LLM-built TOC tree ([]TOCNode, persisted on documents.toc_tree),
//     which carries the document's logical outline with the clean heading
//     vocabulary clients actually expect ("Item 8" → "Balance Sheet") and
//     page anchors, but no content.
//
// Because the two are never reconciled, the map a citation resolves
// against (parser titles) and the map that holds the real headings (the
// TOC) can — and do — diverge. BuildHeadingPaths closes that gap without
// merging the two trees: it returns, for every section, the canonical
// heading path it belongs under, so a citation can carry a real
// structural path instead of a parser chunk label.
//
// # Matching: page-range containment
//
// A section belongs under the TOC node whose effective page span best
// covers the section's own [PageStart, PageEnd]. Among the TOC nodes that
// overlap a section, the winner is chosen by, in order:
//
//  1. containment — a node that fully contains the section beats one that
//     merely overlaps it (the section sits cleanly inside that heading);
//  2. depth — the deeper (more specific) heading wins, so a section under
//     "Item 8 → Balance Sheet" maps to both, ending at "Balance Sheet"
//     rather than stopping at "Item 8";
//  3. overlap — more shared pages wins;
//  4. span — the tighter node wins (more specific);
//  5. start page — earlier wins, purely to make the result deterministic.
//
// # Degradation
//
// Sections with no page range (PageStart/PageEnd <= 0 — the normal state
// for non-paginated formats), and every section when the TOC is empty or
// nil, are simply absent from the returned map. Callers treat a missing
// entry as "no canonical heading path known" and fall back to existing
// behaviour, so wiring this in never makes a citation worse than today.
//
// The returned map is keyed by SectionID and never nil (an empty map is
// returned when nothing could be mapped) so callers can index it without
// a nil check.
func BuildHeadingPaths(root *Section, toc []TOCNode) map[SectionID][]string {
	out := make(map[SectionID][]string)
	if root == nil || len(toc) == 0 {
		return out
	}

	maxPage := documentMaxPage(root, toc)
	entries := flattenTOC(toc, nil, maxPage)
	if len(entries) == 0 {
		return out
	}

	root.Walk(func(s *Section) bool {
		if s == nil || s.PageStart <= 0 || s.PageEnd <= 0 {
			return true
		}
		if path, ok := bestHeadingPath(entries, s.PageStart, s.PageEnd); ok {
			out[s.ID] = path
		}
		return true
	})
	return out
}

// tocEntry is a flattened TOC node with its effective (resolved) page
// span and the full heading path leading to it. depth is the node's
// 0-indexed nesting level, used to prefer more specific headings.
type tocEntry struct {
	start int
	end   int
	depth int
	path  []string
}

// span is the inclusive page count the entry covers. A malformed node
// (end < start) that survived resolution reports the maximum span so it
// sorts last in specificity comparisons, regardless of int width.
func (e tocEntry) span() int {
	if e.end < e.start {
		return math.MaxInt
	}
	return e.end - e.start + 1
}

// flattenTOC walks the TOC forest depth-first, resolving each node's
// effective end page and accumulating the heading path. parentPath is
// the chain of titles above this level; parentEnd bounds open-ended
// nodes (a node whose EndPage is 0 runs until the next sibling's start,
// or — for the last sibling — its parent's end, or the document end).
//
// Empty titles are skipped in the accumulated path so a structural
// wrapper node with no heading doesn't inject a blank segment, but its
// children still inherit the correct ancestry.
func flattenTOC(nodes []TOCNode, parentPath []string, parentEnd int) []tocEntry {
	return flattenTOCAt(nodes, parentPath, parentEnd, 0)
}

func flattenTOCAt(nodes []TOCNode, parentPath []string, parentEnd, depth int) []tocEntry {
	var out []tocEntry
	for i, n := range nodes {
		start := n.StartPage
		end := resolveEndPage(nodes, i, parentEnd)

		path := parentPath
		if t := normaliseTitle(n.Title); t != "" {
			// Copy so sibling branches never share/alias the backing array.
			path = append(append([]string(nil), parentPath...), t)
		}

		if start > 0 && end >= start {
			out = append(out, tocEntry{start: start, end: end, depth: depth, path: path})
		}
		if len(n.Nodes) > 0 {
			// A child can't extend past its parent's end; pass end down so
			// an open-ended deepest child is bounded by its ancestor.
			childBound := end
			if childBound <= 0 {
				childBound = parentEnd
			}
			out = append(out, flattenTOCAt(n.Nodes, path, childBound, depth+1)...)
		}
	}
	return out
}

// resolveEndPage computes the effective inclusive end page for nodes[i].
// An explicit EndPage wins. Otherwise the node runs until the page
// before the next sibling that carries a StartPage; if there is no such
// sibling it inherits parentEnd (the enclosing node's end, or the
// document's last page at the top level).
func resolveEndPage(nodes []TOCNode, i, parentEnd int) int {
	if nodes[i].EndPage > 0 {
		return nodes[i].EndPage
	}
	for j := i + 1; j < len(nodes); j++ {
		if nodes[j].StartPage > 0 {
			if nodes[j].StartPage-1 >= nodes[i].StartPage {
				return nodes[j].StartPage - 1
			}
			return nodes[i].StartPage // degenerate ordering: single page
		}
	}
	if parentEnd > 0 {
		return parentEnd
	}
	return nodes[i].StartPage
}

// bestHeadingPath picks the heading path for a section spanning
// [secStart, secEnd] using the precedence documented on
// BuildHeadingPaths. Returns ok=false when no TOC entry overlaps the
// section at all.
func bestHeadingPath(entries []tocEntry, secStart, secEnd int) ([]string, bool) {
	bestIdx := -1
	for i, e := range entries {
		ov := overlapPages(e.start, e.end, secStart, secEnd)
		if ov <= 0 {
			continue
		}
		if bestIdx < 0 || lessSpecific(entries[bestIdx], e, secStart, secEnd) {
			bestIdx = i
		}
	}
	if bestIdx < 0 || len(entries[bestIdx].path) == 0 {
		return nil, false
	}
	// Defensive copy so callers can't mutate our internal slices.
	return append([]string(nil), entries[bestIdx].path...), true
}

// lessSpecific reports whether the current best entry a is a WORSE match
// for the section than candidate b — i.e. b should replace a. The
// ordering mirrors the BuildHeadingPaths precedence list.
func lessSpecific(a, b tocEntry, secStart, secEnd int) bool {
	aContains := contains(a.start, a.end, secStart, secEnd)
	bContains := contains(b.start, b.end, secStart, secEnd)
	if aContains != bContains {
		return bContains // prefer the container
	}
	if a.depth != b.depth {
		return b.depth > a.depth // prefer deeper / more specific
	}
	aOv := overlapPages(a.start, a.end, secStart, secEnd)
	bOv := overlapPages(b.start, b.end, secStart, secEnd)
	if aOv != bOv {
		return bOv > aOv // prefer more overlap
	}
	if a.span() != b.span() {
		return b.span() < a.span() // prefer the tighter node
	}
	return b.start < a.start // deterministic tie-break
}

// contains reports whether [oStart,oEnd] fully encloses [iStart,iEnd].
func contains(oStart, oEnd, iStart, iEnd int) bool {
	return oStart <= iStart && iEnd <= oEnd
}

// overlapPages returns the count of shared inclusive pages between two
// ranges, or 0 when they don't intersect.
func overlapPages(aStart, aEnd, bStart, bEnd int) int {
	if aStart <= 0 || aEnd <= 0 || bStart <= 0 || bEnd <= 0 {
		return 0
	}
	lo := max(aStart, bStart)
	hi := min(aEnd, bEnd)
	if hi < lo {
		return 0
	}
	return hi - lo + 1
}

// documentMaxPage is the highest page the document is known to reach,
// used to bound open-ended top-level TOC nodes. It takes the max across
// section PageEnds and TOC StartPages so a TOC whose last node has no
// EndPage still resolves to something sane.
func documentMaxPage(root *Section, toc []TOCNode) int {
	hi := 0
	if root != nil {
		root.Walk(func(s *Section) bool {
			if s != nil {
				hi = max(hi, s.PageEnd)
			}
			return true
		})
	}
	var scan func(nodes []TOCNode)
	scan = func(nodes []TOCNode) {
		for _, n := range nodes {
			hi = max(hi, n.EndPage, n.StartPage)
			scan(n.Nodes)
		}
	}
	scan(toc)
	return hi
}

// normaliseTitle trims a TOC title for use as a path segment. It only
// strips surrounding whitespace — the bench's anchor matcher already
// handles case/punctuation/ordinal normalisation, so we keep the
// heading verbatim here and let the consumer normalise for comparison.
func normaliseTitle(s string) string {
	return strings.TrimSpace(s)
}
