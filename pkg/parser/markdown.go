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
			title := strings.TrimSpace(string(h.Text(src)))
			current = &flat{level: h.Level, title: title}
			flats = append(flats, current)
			continue
		}
		appendNodeText(&current.content, n, src)
	}

	// Drop an empty preamble bucket if there's at least one heading.
	if len(flats) > 1 && flats[0].level == 0 && strings.TrimSpace(flats[0].content.String()) == "" {
		flats = flats[1:]
	}

	// Derive document title.
	var title string
	for _, f := range flats {
		if f.level == 1 {
			title = f.title
			break
		}
	}
	if title == "" && len(flats) > 0 {
		title = flats[0].title
	}

	// Second pass: build the hierarchy using a stack of (level, section)
	// pointers. The stack's top is always the most recent ancestor.
	root := &Section{Level: 0, Title: title}
	stack := []*Section{root}

	for _, f := range flats {
		sec := Section{
			Level:   f.level,
			Title:   f.title,
			Content: strings.TrimSpace(f.content.String()),
		}
		if f.level == 0 {
			// Preamble content (before any heading). Hang it off the root
			// as a synthetic "Introduction" section.
			if sec.Content == "" {
				continue
			}
			sec.Level = 1
			sec.Title = "Introduction"
		}
		// Pop until the top of the stack is a strictly shallower level.
		for len(stack) > 1 && stack[len(stack)-1].Level >= sec.Level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, sec)
		// The newly appended child is addressable via the slice tail.
		tail := &parent.Children[len(parent.Children)-1]
		stack = append(stack, tail)
	}

	return &ParsedDoc{
		Title:    title,
		Sections: root.Children,
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
	if _, ok := n.(ast.Node); ok && n.Type() == ast.TypeBlock {
		if !bytes.HasSuffix([]byte(buf.String()), []byte("\n\n")) {
			buf.WriteString("\n\n")
		}
	}
}
