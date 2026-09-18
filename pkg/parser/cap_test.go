package parser

import (
	"strings"
	"testing"
)

// leafTree builds a flat list of n single-leaf sections each carrying
// `size` characters of content, under one shared parent.
func leafTree(n, size int) []Section {
	kids := make([]Section, n)
	for i := range kids {
		kids[i] = Section{Level: 2, Title: "leaf", Content: strings.Repeat("x", size), PageStart: i + 1, PageEnd: i + 1}
	}
	return []Section{{Level: 1, Title: "parent", Children: kids}}
}

// singleLeafParentTree builds n top-level "heading" sections, each an
// internal node with EXACTLY ONE leaf child carrying `size` chars. This
// is the real 10-K explosion shape: hundreds of heading -> one-body-leaf
// parents with NO adjacent leaf-sibling pairs anywhere. The pre-fix cap
// (adjacent-siblings only) could not reduce this tree at all.
func singleLeafParentTree(n, size int) []Section {
	parents := make([]Section, n)
	for i := range parents {
		parents[i] = Section{
			Level: 1,
			Title: "heading",
			Children: []Section{{
				Level:     2,
				Title:     "body",
				Content:   strings.Repeat("y", size),
				PageStart: i + 1,
				PageEnd:   i + 1,
			}},
		}
	}
	return parents
}

func TestCapLeafSections_MergesDownToCap(t *testing.T) {
	tree := leafTree(1000, 50)
	if got := countLeafSections(tree); got != 1000 {
		t.Fatalf("setup: countLeafSections = %d, want 1000", got)
	}
	capped := capLeafSections(tree, 400)
	if got := countLeafSections(capped); got > 400 {
		t.Errorf("after cap: %d leaves, want <= 400", got)
	}
	// No content should be lost — and the absorbed leaf's title is kept
	// as a "**leaf**" heading line, so each merge (< 1000 of them) adds
	// that plus two "\n\n" separators; the total never shrinks.
	orig := 1000 * 50
	perMerge := len("**leaf**") + 4
	if got := totalContentLen(capped); got < orig || got > orig+perMerge*1000 {
		t.Errorf("content not preserved: got %d chars, want in [%d, %d]", got, orig, orig+perMerge*1000)
	}
}

func TestCapLeafSections_UnderCapUnchanged(t *testing.T) {
	tree := leafTree(50, 100)
	capped := capLeafSections(tree, 400)
	if got := countLeafSections(capped); got != 50 {
		t.Errorf("under-cap tree was modified: %d leaves, want 50", got)
	}
}

func TestCapLeafSections_DisabledByNonPositive(t *testing.T) {
	tree := leafTree(1000, 50)
	if got := countLeafSections(capLeafSections(tree, 0)); got != 1000 {
		t.Errorf("maxLeaves=0 should disable cap, got %d leaves", got)
	}
	if got := countLeafSections(capLeafSections(leafTree(1000, 50), -1)); got != 1000 {
		t.Errorf("maxLeaves<0 should disable cap, got %d leaves", got)
	}
}

func TestCapLeafSections_MergesSmallestFirst(t *testing.T) {
	// Two tiny leaves + one large; cap of 2 should merge the two tiny
	// ones and leave the large leaf intact.
	tree := []Section{{Level: 1, Title: "p", Children: []Section{
		{Level: 2, Title: "tiny-a", Content: "aa", PageStart: 1, PageEnd: 1},
		{Level: 2, Title: "tiny-b", Content: "bb", PageStart: 2, PageEnd: 2},
		{Level: 2, Title: "big", Content: strings.Repeat("z", 5000), PageStart: 3, PageEnd: 9},
	}}}
	capped := capLeafSections(tree, 2)
	if got := countLeafSections(capped); got != 2 {
		t.Fatalf("countLeafSections = %d, want 2", got)
	}
	kids := capped[0].Children
	if len(kids) != 2 {
		t.Fatalf("len(kids) = %d, want 2", len(kids))
	}
	// First child is the merged tiny pair; it must carry both bodies and
	// the unioned page range.
	if !strings.Contains(kids[0].Content, "aa") || !strings.Contains(kids[0].Content, "bb") {
		t.Errorf("merged leaf lost content: %q", kids[0].Content)
	}
	if kids[0].PageStart != 1 || kids[0].PageEnd != 2 {
		t.Errorf("merged page range = (%d,%d), want (1,2)", kids[0].PageStart, kids[0].PageEnd)
	}
	// The large leaf survives untouched.
	if len(kids[1].Content) != 5000 {
		t.Errorf("large leaf was merged; len = %d, want 5000", len(kids[1].Content))
	}
}

// TestCapLeafSections_SingleLeafParentsReduce is the regression test for
// the bug that let a 92-page 10-K through at 463-1465 leaves: a tree made
// entirely of single-leaf parents (heading -> one body leaf) has no
// adjacent leaf-sibling pairs, so the old adjacent-only merge did nothing.
// The fix must collapse those chains and merge down to the cap, losing no
// content.
func TestCapLeafSections_SingleLeafParentsReduce(t *testing.T) {
	tree := singleLeafParentTree(1000, 30)
	if got := countLeafSections(tree); got != 1000 {
		t.Fatalf("setup: countLeafSections = %d, want 1000", got)
	}
	orig := totalContentLen(tree) // 1000 * 30

	capped := capLeafSections(tree, 400)

	if got := countLeafSections(capped); got > 400 {
		t.Errorf("after cap: %d leaves, want <= 400 (the bug let 1465 through)", got)
	}
	// No content lost. Every collapse or merge keeps the absorbed title
	// ("**body**" or "**heading**", at most 11 chars) plus two "\n\n"
	// separators; with < 2000 folds the upper bound is generous.
	perFold := len("**heading**") + 4
	if got := totalContentLen(capped); got < orig || got > orig+perFold*2000 {
		t.Errorf("content not preserved: got %d chars, want in [%d, %d]", got, orig, orig+perFold*2000)
	}
}

// TestCapLeafSections_DeepSingleLeafChains exercises heading -> subheading
// -> body chains (depth-3 single-leaf parents). The bottom-up collapse
// must fold the whole chain so the bodies become mergeable siblings.
func TestCapLeafSections_DeepSingleLeafChains(t *testing.T) {
	const n = 600
	roots := make([]Section, n)
	for i := range roots {
		roots[i] = Section{Level: 1, Title: "h1", Children: []Section{{
			Level: 2, Title: "h2", Children: []Section{{
				Level: 3, Title: "body", Content: strings.Repeat("z", 20),
				PageStart: i + 1, PageEnd: i + 1,
			}},
		}}}
	}
	if got := countLeafSections(roots); got != n {
		t.Fatalf("setup: countLeafSections = %d, want %d", got, n)
	}
	orig := totalContentLen(roots)

	capped := capLeafSections(roots, 400)
	if got := countLeafSections(capped); got > 400 {
		t.Errorf("after cap: %d leaves, want <= 400", got)
	}
	if got := totalContentLen(capped); got < orig {
		t.Errorf("content shrank: got %d chars, want >= %d", got, orig)
	}
}

// TestCapLeafSections_MixedShapeReducesToCap throws a tree that mixes a
// flat sibling list, single-leaf parents, and a multi-child branch at the
// cap. The invariant is unconditional: > N mergeable leaves must come
// down to <= N regardless of shape.
func TestCapLeafSections_MixedShapeReducesToCap(t *testing.T) {
	tree := []Section{
		{Level: 1, Title: "flat", Children: func() []Section {
			out := make([]Section, 300)
			for i := range out {
				out[i] = Section{Level: 2, Title: "f", Content: "ff", PageStart: i + 1, PageEnd: i + 1}
			}
			return out
		}()},
	}
	// Append 300 single-leaf parents as additional top-level nodes.
	tree = append(tree, singleLeafParentTree(300, 10)...)

	if got := countLeafSections(tree); got != 600 {
		t.Fatalf("setup: countLeafSections = %d, want 600", got)
	}
	capped := capLeafSections(tree, 100)
	if got := countLeafSections(capped); got > 100 {
		t.Errorf("after cap: %d leaves, want <= 100", got)
	}
}

// TestCapLeafSections_NeverMergesTableSections asserts that table leaves
// (Metadata["table"]="true") are preserved verbatim and never merged or
// collapsed, even under a cap small enough that everything else must
// merge around them.
func TestCapLeafSections_NeverMergesTableSections(t *testing.T) {
	mkTable := func(page int, content string) Section {
		return Section{
			Level: 1, Title: "Table", Content: content,
			PageStart: page, PageEnd: page,
			Metadata: map[string]string{"table": "true"},
		}
	}
	// Three tables interleaved with prose leaves under one parent, plus a
	// pile of single-leaf-parent prose so we're well over the cap.
	kids := []Section{
		{Level: 2, Title: "p1", Content: "prose-a"},
		mkTable(1, "| A | B |\n| --- | --- |\n| 1 | 2 |"),
		{Level: 2, Title: "p2", Content: "prose-b"},
		mkTable(2, "| C | D |\n| --- | --- |\n| 3 | 4 |"),
		{Level: 2, Title: "p3", Content: "prose-c"},
		mkTable(3, "| E | F |\n| --- | --- |\n| 5 | 6 |"),
	}
	tree := []Section{{Level: 1, Title: "parent", Children: kids}}
	tree = append(tree, singleLeafParentTree(200, 10)...)

	capped := capLeafSections(tree, 5)

	// Every original table's content must still be present, verbatim, in
	// some leaf.
	tableContents := []string{
		"| A | B |\n| --- | --- |\n| 1 | 2 |",
		"| C | D |\n| --- | --- |\n| 3 | 4 |",
		"| E | F |\n| --- | --- |\n| 5 | 6 |",
	}
	var leaves []Section
	var collect func([]Section)
	collect = func(ss []Section) {
		for i := range ss {
			if len(ss[i].Children) == 0 {
				leaves = append(leaves, ss[i])
			} else {
				collect(ss[i].Children)
			}
		}
	}
	collect(capped)

	for _, want := range tableContents {
		found := false
		for _, l := range leaves {
			// A table must survive as its OWN leaf — its content present and
			// the table metadata intact (i.e. it wasn't merged into prose).
			if l.Content == want && l.Metadata["table"] == "true" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("table content %q was merged away or lost its metadata", want)
		}
	}
}

func totalContentLen(sections []Section) int {
	n := 0
	for i := range sections {
		n += len(sections[i].Content)
		n += totalContentLen(sections[i].Children)
	}
	return n
}

// Merging two leaves under the cap keeps the absorbed leaf's title as a
// heading line in the merged body. A 10-K's "Item 2. Properties" used to
// disappear after the one-word "Item 1B. Unresolved Staff Comments".
func TestCapLeafSections_MergeKeepsTheAbsorbedTitle(t *testing.T) {
	tree := []Section{{Level: 1, Title: "PART I", Children: []Section{
		{Level: 2, Title: "Item 1B. Unresolved Staff Comments", Content: "None.", PageStart: 20, PageEnd: 20},
		{Level: 2, Title: "Item 2. Properties", Content: "We consider our plants suitable.", PageStart: 20, PageEnd: 20},
		{Level: 2, Title: "Item 7. MD&A", Content: strings.Repeat("z", 5000), PageStart: 22, PageEnd: 40},
	}}}
	out := capLeafSections(tree, 2)
	kids := out[0].Children
	if len(kids) != 2 {
		t.Fatalf("want 2 leaves after cap, got %d", len(kids))
	}
	m := kids[0]
	if m.Title != "Item 1B. Unresolved Staff Comments" {
		t.Errorf("survivor title changed: %q", m.Title)
	}
	want := "None.\n\n**Item 2. Properties**\n\nWe consider our plants suitable."
	if m.Content != want {
		t.Errorf("merged content\n got %q\nwant %q", m.Content, want)
	}
}

func TestAbsorbChildIntoParentKeepsTheChildTitle(t *testing.T) {
	p := Section{Title: "PART I", Content: "", PageStart: 3}
	absorbChildIntoParent(&p, Section{Title: "Item 1. Business", Content: "Founded in 1982.", PageStart: 3, PageEnd: 5})
	if want := "**Item 1. Business**\n\nFounded in 1982."; p.Content != want {
		t.Errorf("got %q want %q", p.Content, want)
	}
	if p.PageEnd != 5 {
		t.Errorf("page end not unioned: %d", p.PageEnd)
	}
}
