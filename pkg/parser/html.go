package parser

import (
	"context"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTML parses HTML using the same heading-driven section model as the
// Markdown parser: each <h1>-<h6> opens a new section whose content is
// the rendered plain text of everything up to the next heading at the
// same or shallower level.
//
// Non-structural chrome (script, style, nav, header, footer, aside) is
// skipped entirely. The parser prefers <main> / <article> as the root
// when one is present so site-wide navigation doesn't pollute the tree.
type HTML struct{}

// NewHTML returns a new HTML parser.
func NewHTML() *HTML { return &HTML{} }

// Name implements Parser.
func (*HTML) Name() string { return "html" }

// Accepts implements Parser.
func (*HTML) Accepts(contentType, filename string) bool {
	switch contentType {
	case "text/html", "application/xhtml+xml":
		return true
	}
	return HasExt(filename, ".html", ".htm", ".xhtml")
}

// Parse implements Parser.
func (*HTML) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	root, err := html.Parse(r)
	if err != nil {
		return nil, err
	}

	docTitle := findTitle(root)
	content := findMainContent(root)
	if content == nil {
		content = root
	}

	type flat struct {
		level   int
		title   string
		content strings.Builder
	}
	flats := []*flat{{level: 0, title: ""}} // preamble bucket
	current := flats[0]

	// Walk depth-first. On a heading, push a new bucket. On text, append
	// to current. Skip chrome and containers of their own content (we
	// recurse into them so we don't lose their text).
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n == nil || isChrome(n) {
			return
		}
		if lvl := headingLevel(n); lvl > 0 {
			title := strings.TrimSpace(textContent(n))
			current = &flat{level: lvl, title: title}
			flats = append(flats, current)
			return // don't re-emit the heading text into the body
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Table {
			// Render the table as a Markdown grid so cell values keep their
			// row/column labels instead of flattening into loose prose.
			if md := renderTableMarkdown(htmlTableRows(n)); md != "" {
				b := current.content.String()
				if b != "" && !strings.HasSuffix(b, "\n\n") {
					current.content.WriteString("\n\n")
				}
				current.content.WriteString(md)
				current.content.WriteString("\n\n")
			}
			return // handled whole subtree
		}
		if n.Type == html.TextNode {
			t := strings.TrimSpace(n.Data)
			if t != "" {
				if current.content.Len() > 0 {
					current.content.WriteByte(' ')
				}
				current.content.WriteString(t)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		// Insert a blank line after block-level elements for readability.
		if isBlock(n) {
			b := current.content.String()
			if !strings.HasSuffix(b, "\n\n") {
				current.content.WriteString("\n\n")
			}
		}
	}
	walk(content)

	// Convert to the shared flat representation and build the tree with the
	// common hierarchy builder (same algorithm as Markdown/DOCX).
	out := make([]flatSection, 0, len(flats))
	for _, f := range flats {
		out = append(out, flatSection{
			Level:   f.level,
			Title:   f.title,
			Content: cleanWhitespace(f.content.String()),
		})
	}
	out = dropEmptyPreamble(out)

	// Title: prefer the document <title>, then fall back to the first H1 /
	// first bucket via the shared heuristic.
	title := docTitle
	if title == "" {
		title = deriveTitle(out)
	}

	return &ParsedDoc{
		Title:    title,
		Sections: buildSections(out),
	}, nil
}

// headingLevel returns 1-6 for <h1>..<h6>, 0 otherwise.
func headingLevel(n *html.Node) int {
	if n.Type != html.ElementNode {
		return 0
	}
	switch n.DataAtom {
	case atom.H1:
		return 1
	case atom.H2:
		return 2
	case atom.H3:
		return 3
	case atom.H4:
		return 4
	case atom.H5:
		return 5
	case atom.H6:
		return 6
	}
	return 0
}

// isChrome returns true for elements we never want to emit into the body.
func isChrome(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template,
		atom.Nav, atom.Header, atom.Footer, atom.Aside:
		return true
	}
	return false
}

// isBlock returns true for elements that should introduce a paragraph
// break when rendered to plain text.
func isBlock(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.DataAtom {
	case atom.P, atom.Div, atom.Section, atom.Article, atom.Li, atom.Ul,
		atom.Ol, atom.Pre, atom.Blockquote, atom.Tr, atom.Td, atom.Th,
		atom.Table, atom.Br:
		return true
	}
	return false
}

// htmlTableRows extracts a <table> into a rows×cols grid. Each <tr>
// becomes a row; each <td>/<th> becomes a cell with its descendant text
// collapsed. Works through <thead>/<tbody>/<tfoot> wrappers.
func htmlTableRows(table *html.Node) [][]string {
	var rows [][]string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Tr {
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.DataAtom == atom.Td || c.DataAtom == atom.Th) {
					cells = append(cells, cleanWhitespace(textContent(c)))
				}
			}
			rows = append(rows, cells)
			return // don't descend into nested tables as part of this row
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)
	return rows
}

// findTitle returns the text of <title> if present.
func findTitle(root *html.Node) string {
	var title string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if title != "" || n == nil {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Title {
			title = strings.TrimSpace(textContent(n))
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return title
}

// findMainContent prefers <main>, then <article>, then <body>.
func findMainContent(root *html.Node) *html.Node {
	var main, article, body *html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Main:
				if main == nil {
					main = n
				}
			case atom.Article:
				if article == nil {
					article = n
				}
			case atom.Body:
				if body == nil {
					body = n
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	switch {
	case main != nil:
		return main
	case article != nil:
		return article
	case body != nil:
		return body
	}
	return nil
}

// textContent returns the concatenated text of n and its descendants.
func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// cleanWhitespace collapses runs of internal whitespace while preserving
// paragraph breaks.
func cleanWhitespace(s string) string {
	paras := strings.Split(s, "\n\n")
	out := make([]string, 0, len(paras))
	for _, p := range paras {
		p = strings.TrimSpace(strings.Join(strings.Fields(p), " "))
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}
