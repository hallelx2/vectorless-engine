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

// RenderTOCLLMSTxt renders a document's verified table of contents as
// llms.txt (https://llmstxt.org): an H1 title, a blockquote saying what
// the file is, then a "Contents" section listing every entry as a
// Markdown link to the text of its own pages, nested by depth, with its
// page range and summary. An agent reads the map, picks an entry, and
// follows one link to exactly those pages.
//
// pageURL builds the link for a page range. pageCount is the document's
// last page, used for the summary line and to close the last entry's
// range; zero when unknown.
func RenderTOCLLMSTxt(title string, toc []TOCNode, pageCount int, pageURL func(from, to int) string) string {
	var b strings.Builder
	title = oneLine(title)
	if title == "" {
		title = "Document"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	entries := 0
	var count func([]TOCNode)
	count = func(ns []TOCNode) {
		for _, n := range ns {
			entries++
			count(n.Nodes)
		}
	}
	count(toc)
	desc := fmt.Sprintf("%d contents entries", entries)
	if pageCount > 0 {
		desc = fmt.Sprintf("%d pages, %d contents entries", pageCount, entries)
	}
	fmt.Fprintf(&b, "> The document's own table of contents, %s, each placed on the page where it begins. Every entry links to the text of its pages.\n\n## Contents\n\n", desc)

	var walk func(ns []TOCNode, depth, ceiling int)
	walk = func(ns []TOCNode, depth, ceiling int) {
		for i, n := range ns {
			from, to := n.StartPage, n.EndPage
			if to == 0 {
				// Open-ended: runs to the next sibling, else the parent's end.
				to = ceiling
				if i+1 < len(ns) && ns[i+1].StartPage > 0 {
					to = ns[i+1].StartPage - 1
				}
			}
			label := oneLine(n.Title)
			if label == "" {
				label = n.Structure
			}
			line := label
			pages := ""
			switch {
			case from > 0 && to > from:
				pages = fmt.Sprintf("pp. %d–%d", from, to)
				line = fmt.Sprintf("[%s](%s)", label, pageURL(from, to))
			case from > 0:
				pages = fmt.Sprintf("p. %d", from)
				line = fmt.Sprintf("[%s](%s)", label, pageURL(from, from))
			}
			fmt.Fprintf(&b, "%s- %s", strings.Repeat("  ", depth), line)
			if pages != "" {
				fmt.Fprintf(&b, ": %s", pages)
			}
			if s := oneLine(n.Summary); s != "" {
				fmt.Fprintf(&b, " — %s", s)
			}
			b.WriteString("\n")
			childCeiling := to
			if childCeiling == 0 {
				childCeiling = ceiling
			}
			walk(n.Nodes, depth+1, childCeiling)
		}
	}
	walk(toc, 0, pageCount)
	return b.String()
}
