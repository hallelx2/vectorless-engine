package parser

import (
	"strings"
	"testing"
)

func TestChunkOversizedLeavesSplits(t *testing.T) {
	// 12 words per "sentence", 5 sentences ~ 60-65 words, ~360 chars; we want
	// >2400 chars so build it from a longer paragraph + a colon-terminated header.
	header := "Securities registered pursuant to Section 12(b) of the Act: "
	long := strings.Repeat("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu ", 60)
	content := header + long
	if len(content) <= leafChunkThreshold {
		t.Fatalf("test setup: content must exceed threshold; got %d", len(content))
	}
	in := []Section{{Level: 1, Title: "3M COMPANY", Content: content}}

	out := chunkOversizedLeaves(in)
	if len(out) != 1 {
		t.Fatalf("expected 1 top-level section, got %d", len(out))
	}
	parent := out[0]
	if parent.Title != "3M COMPANY" {
		t.Errorf("parent title should be preserved, got %q", parent.Title)
	}
	if parent.Content != "" {
		t.Errorf("parent content should be cleared after splitting, got %d chars", len(parent.Content))
	}
	if len(parent.Children) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(parent.Children))
	}
	// First chunk's title should use the colon-terminated header.
	if !strings.HasPrefix(parent.Children[0].Title, "Securities registered pursuant to Section 12(b)") {
		t.Errorf("first chunk title should come from the colon header, got %q", parent.Children[0].Title)
	}
	// Every chunk's content should be non-empty and well below the original.
	for i, c := range parent.Children {
		if c.Content == "" {
			t.Errorf("chunk %d has empty content", i)
		}
		if len(c.Content) > leafChunkTarget*2 {
			t.Errorf("chunk %d larger than expected: %d chars", i, len(c.Content))
		}
	}
}

func TestChunkOversizedLeavesLeavesSmallSectionsAlone(t *testing.T) {
	in := []Section{
		{Level: 1, Title: "Intro", Content: strings.Repeat("a b c d e f ", 50)},  // ~600 chars
		{Level: 1, Title: "Methods", Content: strings.Repeat("x y z ", 200)},      // ~1200 chars
	}
	out := chunkOversizedLeaves(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 sections preserved, got %d", len(out))
	}
	for i, s := range out {
		if len(s.Children) != 0 {
			t.Errorf("section %d was unexpectedly split into %d children", i, len(s.Children))
		}
	}
}

func TestChunkOversizedLeavesRecursesIntoInternals(t *testing.T) {
	bigLeaf := Section{Level: 2, Title: "Detail", Content: strings.Repeat("the quick brown fox jumps over the lazy dog ", 100)}
	parent := Section{Level: 1, Title: "Parent", Children: []Section{bigLeaf}}
	out := chunkOversizedLeaves([]Section{parent})
	if len(out) != 1 || len(out[0].Children) == 0 {
		t.Fatalf("parent should be retained with chunked children, got %+v", out)
	}
	leaf := out[0].Children[0]
	if leaf.Title != "Detail" {
		t.Errorf("inner leaf title should be preserved, got %q", leaf.Title)
	}
	if len(leaf.Children) < 2 {
		t.Errorf("inner leaf should have been chunked, has %d children", len(leaf.Children))
	}
}

func TestDeriveChunkTitleColonHeader(t *testing.T) {
	got := deriveChunkTitle("Securities registered pursuant to Section 12(b) of the Act: Title of each class ...", "fallback")
	want := "Securities registered pursuant to Section 12(b) of the Act"
	if got != want {
		t.Errorf("colon-header title: got %q want %q", got, want)
	}
}

func TestDeriveChunkTitleFallback(t *testing.T) {
	if got := deriveChunkTitle("", "fb"); got != "fb" {
		t.Errorf("empty chunk should fall back, got %q", got)
	}
}
