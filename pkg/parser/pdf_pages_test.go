package parser

import "testing"

// TestPropagateSectionPagesUnion checks that an internal node with empty
// own pages inherits the union of its descendant leaves' ranges. Pages
// move from leaves UP — never down — so a leaf with explicit pages keeps
// them untouched.
func TestPropagateSectionPagesUnion(t *testing.T) {
	in := []Section{{
		Title: "Chapter 1", // no own range
		Children: []Section{
			{Title: "1.1", PageStart: 2, PageEnd: 4},
			{Title: "1.2", PageStart: 5, PageEnd: 7},
		},
	}}
	propagateSectionPages(in)

	if in[0].PageStart != 2 || in[0].PageEnd != 7 {
		t.Errorf("internal node should span children: got pages %d-%d, want 2-7",
			in[0].PageStart, in[0].PageEnd)
	}
	// Children unchanged.
	if c := in[0].Children[0]; c.PageStart != 2 || c.PageEnd != 4 {
		t.Errorf("child 1 mutated: %d-%d", c.PageStart, c.PageEnd)
	}
}

// TestPropagateSectionPagesIgnoresZero ensures sections with NO known
// page info (the markdown/HTML case) don't get spurious zero ranges
// painted on by propagation — zero stays zero.
func TestPropagateSectionPagesIgnoresZero(t *testing.T) {
	in := []Section{{
		Title: "Chapter 1",
		Children: []Section{
			{Title: "Leaf A"}, // no pages anywhere
			{Title: "Leaf B"},
		},
	}}
	propagateSectionPages(in)
	if in[0].PageStart != 0 || in[0].PageEnd != 0 {
		t.Errorf("propagation should leave zero ranges alone, got %d-%d",
			in[0].PageStart, in[0].PageEnd)
	}
}

// TestPropagateSectionPagesMixedZeroAndKnown checks that a tree where
// only some leaves have pages still produces a sensible span on parents.
func TestPropagateSectionPagesMixedZeroAndKnown(t *testing.T) {
	in := []Section{{
		Title: "Chapter 1",
		Children: []Section{
			{Title: "Leaf A"}, // unknown
			{Title: "Leaf B", PageStart: 5, PageEnd: 8},
		},
	}}
	propagateSectionPages(in)
	if in[0].PageStart != 5 || in[0].PageEnd != 8 {
		t.Errorf("parent should equal the only known leaf range: got %d-%d, want 5-8",
			in[0].PageStart, in[0].PageEnd)
	}
}

// TestPropagateSectionPagesParentWidens makes sure a parent's own range
// is widened when its children straddle further.
func TestPropagateSectionPagesParentWidens(t *testing.T) {
	in := []Section{{
		Title:     "Chapter 1",
		PageStart: 3,
		PageEnd:   3,
		Children: []Section{
			{Title: "Leaf A", PageStart: 5, PageEnd: 8},
		},
	}}
	propagateSectionPages(in)
	if in[0].PageStart != 3 || in[0].PageEnd != 8 {
		t.Errorf("parent should span its own + children: got %d-%d, want 3-8",
			in[0].PageStart, in[0].PageEnd)
	}
}

// TestChunkOversizedLeavesInheritsPages confirms that when a too-long
// leaf gets split into sub-chunks, every sub-chunk inherits the parent
// leaf's page range (we don't re-derive pages from byte offsets — that
// would lie about precision).
func TestChunkOversizedLeavesInheritsPages(t *testing.T) {
	const longContent = "alpha beta gamma delta epsilon zeta eta theta iota kappa "
	// 2400-char threshold => need >2400 chars
	long := ""
	for len(long) <= leafChunkThreshold {
		long += longContent
	}
	in := []Section{{
		Level:     1,
		Title:     "Big Leaf",
		Content:   long,
		PageStart: 12,
		PageEnd:   17,
	}}
	out := chunkOversizedLeaves(in)
	if len(out) != 1 {
		t.Fatalf("expected 1 top-level section, got %d", len(out))
	}
	parent := out[0]
	if parent.PageStart != 12 || parent.PageEnd != 17 {
		t.Errorf("parent should keep its page range, got %d-%d", parent.PageStart, parent.PageEnd)
	}
	if len(parent.Children) < 2 {
		t.Fatalf("expected chunks, got %d", len(parent.Children))
	}
	for i, c := range parent.Children {
		if c.PageStart != 12 || c.PageEnd != 17 {
			t.Errorf("chunk %d should inherit pages 12-17, got %d-%d",
				i, c.PageStart, c.PageEnd)
		}
	}
}
