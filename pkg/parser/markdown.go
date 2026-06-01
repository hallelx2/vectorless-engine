package parser

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Markdown parses Markdown using goldmark. Section boundaries are
// determined by ATX/Setext headings: every heading opens a new section
// whose content is all nodes up to (but not including) the next heading
// at the same or shallower level.
type Markdown struct {
	gm goldmark.Markdown
}

// NewMarkdown returns a Markdown parser ready to use.
func NewMarkdown() *Markdown {
	return &Markdown{gm: goldmark.New()}
}

// Name implements Parser.
func (*Markdown) Name() string { return "markdown" }

// Accepts implements Parser.
func (*Markdown) Accepts(contentType, filename string) bool {
	switch contentType {
	case "text/markdown", "text/x-markdown":
		return true
	}
	return HasExt(filename, ".md", ".markdown")
}

// Parse implements Parser.
func (m *Markdown) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	reader := text.NewReader(src)
	doc := m.gm.Parser().Parse(reader)

	// First pass: flatten (level, title, content) tuples in document order.
	type flat struct {
		level   int
		title   string
		content strings.Builder
	}
	var flats []*flat
	current := &flat{level: 0, title: ""} // preamble bucket
	flats = append(flats, current)

	// Walk top-level children. For each heading, start a new bucket; for
	// anything else, append its rendered text to the current bucket.
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok {
			title := strings.TrimSpace(headingText(h, src))
			current = &flat{level: h.Level, title: title}
			flats = append(flats, current)
			continue
		}
		appendNodeText(&current.content, n, src)
	}

	// Convert to the shared flat representation, then let the common
	// hierarchy builder shape the tree (identical across Markdown/HTML/DOCX).
	out := make([]flatSection, 0, len(flats))
	for _, f := range flats {
		out = append(out, flatSection{
			Level:   f.level,
			Title:   f.title,
			Content: strings.TrimSpace(f.content.String()),
		})
	}
	out = dropEmptyPreamble(out)
	title := deriveTitle(out)

	return &ParsedDoc{
		Title:    title,
		Sections: buildSections(out),
	}, nil
}

// appendNodeText walks a node's subtree and appends its rendered text
// into buf. This gives us a plain-text body per section — good enough
// for the LLM to read; richer rendering (preserving lists, code blocks)
// can come later.
func appendNodeText(buf *strings.Builder, n ast.Node, src []byte) {
	switch v := n.(type) {
	case *ast.Text:
		buf.Write(v.Segment.Value(src))
	case *ast.CodeBlock, *ast.FencedCodeBlock:
		// Render code blocks literally, preserving line breaks.
		lines := v.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			buf.Write(seg.Value(src))
		}
		buf.WriteString("\n")
		return
	}

	// Recurse into children.
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		appendNodeText(buf, c, src)
	}

	// Paragraph-ish block boundaries: add a blank line after block nodes
	// so the flattened text still reads.
	if n.Type() == ast.TypeBlock {
		if !bytes.HasSuffix([]byte(buf.String()), []byte("\n\n")) {
			buf.WriteString("\n\n")
		}
	}
}

// headingText extracts the flattened text of a heading by walking its
// inline children. Replaces the deprecated Heading.Text(src) API.
func headingText(h *ast.Heading, src []byte) string {
	var buf bytes.Buffer
	for c := h.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			buf.Write(t.Segment.Value(src))
			continue
		}
		// Recurse for wrapping inlines (emphasis, links, code spans, etc.)
		for gc := c.FirstChild(); gc != nil; gc = gc.NextSibling() {
			if t, ok := gc.(*ast.Text); ok {
				buf.Write(t.Segment.Value(src))
			}
		}
	}
	return buf.String()
}
