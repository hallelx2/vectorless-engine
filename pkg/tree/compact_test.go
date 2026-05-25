package tree

import (
	"testing"
)

func buildLargeTestTree() *Tree {
	return &Tree{
		DocumentID: "doc_compact",
		Title:      "Compact Test",
		Root: &Section{
			ID: "root", Title: "Root",
			Children: []*Section{
				{
					ID: "ch1", ParentID: "root", Title: "Chapter 1",
					Children: []*Section{
						{ID: "ch1a", ParentID: "ch1", Title: "Section 1A", TokenCount: 200, ContentRef: "a.txt"},
						{ID: "ch1b", ParentID: "ch1", Title: "Section 1B", TokenCount: 30, ContentRef: "b.txt"},  // small
						{ID: "ch1c", ParentID: "ch1", Title: "Section 1C", TokenCount: 10, ContentRef: "c.txt"},  // tiny
					},
				},
				{
					ID: "ch2", ParentID: "root", Title: "Chapter 2",
					Children: []*Section{
						{ID: "ch2a", ParentID: "ch2", Title: "Only Child", TokenCount: 500, ContentRef: "d.txt"},
					},
				},
				{ID: "ch3", ParentID: "root", Title: "Appendix", TokenCount: 5, ContentRef: "e.txt"}, // tiny leaf
			},
		},
	}
}

func TestCompactPrunesSmallLeaves(t *testing.T) {
	t.Parallel()
	tr := buildLargeTestTree()

	compacted := tr.Compact(CompactOpts{
		MinTokens:        50,
		MergeSingleChild: false,
	})

	// ch1c (10 tokens) and ch1b (30 tokens) should be pruned.
	// ch3 (5 tokens) should be pruned.
	if s := compacted.FindByID("ch1c"); s != nil {
		t.Error("ch1c (10 tokens) should be pruned")
	}
	if s := compacted.FindByID("ch1b"); s != nil {
		t.Error("ch1b (30 tokens) should be pruned")
	}
	if s := compacted.FindByID("ch3"); s != nil {
		t.Error("ch3 (5 tokens) should be pruned")
	}

	// ch1a (200 tokens) and ch2a (500 tokens) should survive.
	if s := compacted.FindByID("ch1a"); s == nil {
		t.Error("ch1a (200 tokens) should survive")
	}
	if s := compacted.FindByID("ch2a"); s == nil {
		t.Error("ch2a (500 tokens) should survive")
	}
}

func TestCompactMergeSingleChild(t *testing.T) {
	t.Parallel()
	tr := buildLargeTestTree()

	compacted := tr.Compact(CompactOpts{
		MinTokens:        50,
		MergeSingleChild: true,
	})

	// After pruning: ch2 has only one child (ch2a). Should be merged.
	// ch2a should now have title "Chapter 2 > Only Child".
	found := false
	compacted.Root.Walk(func(s *Section) bool {
		if s.Title == "Chapter 2 > Only Child" {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Error("expected merged title 'Chapter 2 > Only Child'")
	}
}

func TestCompactMaxDepth(t *testing.T) {
	t.Parallel()
	tr := buildLargeTestTree()

	compacted := tr.Compact(CompactOpts{
		MinTokens:        0, // no pruning
		MergeSingleChild: false,
		MaxDepth:          2, // root(0) + chapters(1) only
	})

	// All sections at depth >= 2 should be flattened.
	if s := compacted.FindByID("ch1a"); s != nil {
		t.Error("ch1a should be flattened into ch1")
	}
	if s := compacted.FindByID("ch2a"); s != nil {
		t.Error("ch2a should be flattened into ch2")
	}

	// Chapters should still exist.
	if s := compacted.FindByID("ch1"); s == nil {
		t.Error("ch1 should survive")
	}
}

func TestCompactPreservesOriginal(t *testing.T) {
	t.Parallel()
	tr := buildLargeTestTree()
	originalCount := tr.SectionCount()

	_ = tr.Compact(CompactOpts{MinTokens: 50})

	// Original tree should be unchanged.
	if tr.SectionCount() != originalCount {
		t.Errorf("original tree modified: was %d, now %d sections", originalCount, tr.SectionCount())
	}
}

func TestCompactNilTree(t *testing.T) {
	t.Parallel()
	var tr *Tree
	result := tr.Compact(CompactOpts{})
	if result != nil {
		t.Error("compact of nil tree should return nil")
	}
}

func TestSectionCount(t *testing.T) {
	t.Parallel()
	tr := buildLargeTestTree()
	// root + ch1 + ch1a + ch1b + ch1c + ch2 + ch2a + ch3 = 8
	if n := tr.SectionCount(); n != 8 {
		t.Errorf("section count = %d, want 8", n)
	}
}

func TestTotalTokens(t *testing.T) {
	t.Parallel()
	tr := buildLargeTestTree()
	// 200 + 30 + 10 + 500 + 5 = 745
	if n := tr.TotalTokens(); n != 745 {
		t.Errorf("total tokens = %d, want 745", n)
	}
}

func TestCompactDefaultMinTokens(t *testing.T) {
	t.Parallel()
	tr := buildLargeTestTree()
	// Default MinTokens should be 50 when 0 is passed.
	compacted := tr.Compact(CompactOpts{})
	// ch1b (30), ch1c (10), ch3 (5) should be pruned with default 50.
	if s := compacted.FindByID("ch1c"); s != nil {
		t.Error("ch1c should be pruned with default MinTokens")
	}
}
