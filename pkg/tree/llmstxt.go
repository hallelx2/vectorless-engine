package tree

import (
	"fmt"
	"strings"
)

// RenderLLMSTxt renders the tree as an llms.txt-style Markdown map: an H1
// title, a blockquote document summary, then a nested heading outline
// where every section carries its one-line summary.
//
// This is the document's "navigable map" in the emerging llms.txt
// convention (https://llmstxt.org) — a compact, LLM-friendly index an
// agent can read to decide which sections to pull in full. It is exactly
// what the vectorless tree already is, serialized to the standard format.
func (t *Tree) RenderLLMSTxt() string {
	var b strings.Builder

	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = string(t.DocumentID)
	}
	fmt.Fprintf(&b, "# %s\n", title)

	if t.Root == nil {
		return b.String()
	}

	// Document-level summary as a blockquote.
	if s := oneLine(t.Root.Summary); s != "" {
		fmt.Fprintf(&b, "\n> %s\n", s)
	}

	// The root is the title node; its children are the real sections.
	for _, c := range t.Root.Children {
		writeLLMSSection(&b, c, 2)
	}

	return b.String()
}

// writeLLMSSection writes one section as a Markdown heading (clamped to
// h6) followed by its summary, then recurses into children one level
// deeper.
func writeLLMSSection(b *strings.Builder, s *Section, level int) {
	if s == nil {
		return
	}
	if level > 6 {
		level = 6
	}

	heading := strings.TrimSpace(s.Title)
	if heading == "" {
		heading = string(s.ID)
	}
	fmt.Fprintf(b, "\n%s %s\n", strings.Repeat("#", level), heading)

	if sum := oneLine(s.Summary); sum != "" {
		fmt.Fprintf(b, "%s\n", sum)
	}

	for _, c := range s.Children {
		writeLLMSSection(b, c, level+1)
	}
}

// oneLine collapses all internal whitespace (including newlines) into
// single spaces so a multi-line summary renders as one clean line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
