package parser

import (
	"strings"
	"testing"
)

// TestFoldEmptyLeafSections proves the post-parse pass eliminates empty
// "structural" leaf fragments — the multi-line display-title remnants and
// running headers that otherwise ship as sections returning empty content
// to the browser / API — while preserving their title text on the sibling
// body they head, and never touching internal nodes (empty headings that
// carry real sub-sections).
func TestFoldEmptyLeafSections(t *testing.T) {
	t.Parallel()

	t.Run("empty leaf folds its title into the following sibling", func(t *testing.T) {
		in := []Section{
			{Level: 1, Title: "How Vectorless", Content: "", PageStart: 3, PageEnd: 3},
			{Level: 1, Title: "Body", Content: "Vectorless walks the document tree.", PageStart: 3, PageEnd: 4},
		}
		out := foldEmptyLeafSections(in)
		if len(out) != 1 {
			t.Fatalf("want 1 section (empty leaf folded away), got %d: %+v", len(out), out)
		}
		if strings.Contains(out[0].Content, "walks the document tree") == false {
			t.Errorf("body content lost: %q", out[0].Content)
		}
		if !strings.Contains(out[0].Content, "How Vectorless") {
			t.Errorf("folded title not preserved in content: %q", out[0].Content)
		}
		if out[0].PageStart != 3 {
			t.Errorf("PageStart should union to 3, got %d", out[0].PageStart)
		}
	})

	t.Run("consecutive empty fragments accumulate onto one body", func(t *testing.T) {
		secs := []Section{
			{Level: 1, Title: "THE SYSTEM, END-TO-END", Content: "", PageStart: 2, PageEnd: 2},
			{Level: 1, Title: "How Vectorless", Content: "", PageStart: 2, PageEnd: 2},
			{Level: 1, Title: "actually works.", Content: "", PageStart: 2, PageEnd: 2},
			{Level: 1, Title: "Intro", Content: "The system reads structure first.", PageStart: 2, PageEnd: 3},
		}
		out := foldEmptyLeafSections(secs)
		if len(out) != 1 {
			t.Fatalf("want 1 section, got %d: %+v", len(out), out)
		}
		for _, frag := range []string{"THE SYSTEM", "How Vectorless", "actually works", "reads structure first"} {
			if !strings.Contains(out[0].Content, frag) {
				t.Errorf("fragment %q missing from folded content: %q", frag, out[0].Content)
			}
		}
	})

	t.Run("empty internal node (heading with children) is preserved", func(t *testing.T) {
		in := []Section{
			{Level: 1, Title: "Chapter", Content: "", Children: []Section{
				{Level: 2, Title: "Sub", Content: "real body", PageStart: 5, PageEnd: 5},
			}},
		}
		out := foldEmptyLeafSections(in)
		if len(out) != 1 || len(out[0].Children) != 1 {
			t.Fatalf("internal node must survive with its child, got %+v", out)
		}
	})

	t.Run("trailing empty fragment with no following body attaches to previous", func(t *testing.T) {
		in := []Section{
			{Level: 1, Title: "Body", Content: "content here", PageStart: 1, PageEnd: 1},
			{Level: 1, Title: "Dangling Header", Content: "", PageStart: 2, PageEnd: 2},
		}
		out := foldEmptyLeafSections(in)
		if len(out) != 1 {
			t.Fatalf("want 1 section, got %d", len(out))
		}
		if !strings.Contains(out[0].Content, "Dangling Header") {
			t.Errorf("trailing title lost: %q", out[0].Content)
		}
	})

	t.Run("lone empty leaf with no siblings is kept (nothing to fold into)", func(t *testing.T) {
		in := []Section{{Level: 1, Title: "Only", Content: ""}}
		out := foldEmptyLeafSections(in)
		if len(out) != 1 {
			t.Fatalf("lone empty leaf should be kept, got %d", len(out))
		}
	})
}
