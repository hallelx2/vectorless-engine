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

func totalContentLen(sections []Section) int {
	n := 0
	for i := range sections {
		n += len(sections[i].Content)
		n += totalContentLen(sections[i].Children)
	}
	return n
}
