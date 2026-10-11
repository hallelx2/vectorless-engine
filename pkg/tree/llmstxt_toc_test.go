package tree

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderTOCLLMSTxt(t *testing.T) {
	toc := []TOCNode{
		{Title: "PART I", StartPage: 3, EndPage: 9, Nodes: []TOCNode{
			{Title: "Item 1. Business", StartPage: 3, Summary: "What the company makes\nand sells."},
			{Title: "Item 1A. Risk Factors", StartPage: 6},
		}},
		{Title: "Signatures", StartPage: 12, Nodes: []TOCNode{{Title: "CASE 01"}}},
	}
	got := RenderTOCLLMSTxt("ACME  10-K", toc, 12, func(a, b int) string { return fmt.Sprintf("/text?pages=%d-%d", a, b) })
	for _, want := range []string{
		"# ACME 10-K\n",
		"> The document's own table of contents, 12 pages, 5 contents entries",
		"## Contents\n",
		"- [PART I](/text?pages=3-9): pp. 3–9\n",
		"  - [Item 1. Business](/text?pages=3-5): pp. 3–5 — What the company makes and sells.\n",
		"  - [Item 1A. Risk Factors](/text?pages=6-9): pp. 6–9\n",
		"- [Signatures](/text?pages=12-12): p. 12\n",
		"  - [CASE 01](/text?pages=12-12): within p. 12\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
