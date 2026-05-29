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
	// No content should be lost. Merges insert a "\n\n" separator between
	// two non-empty bodies, so the total grows by at most 2 chars per
	// merge (< 1000 merges) but never shrinks below the original.
	orig := 1000 * 50
	if got := totalContentLen(capped); got < orig || got > orig+2*1000 {
		t.Errorf("content not preserved: got %d chars, want in [%d, %d]", got, orig, orig+2*1000)
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
	// No content lost. Collapses/merges insert at most a "\n\n" (2 chars)
	// per fold; with < 1000 folds total the upper bound is generous.
	if got := totalContentLen(capped); got < orig || got > orig+2*1000 {
		t.Errorf("content not preserved: got %d chars, want in [%d, %d]", got, orig, orig+2*1000)
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
