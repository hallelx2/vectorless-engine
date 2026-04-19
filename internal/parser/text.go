package parser

import (
	"context"
	"io"
	"strings"
)

// Text is the trivial plain-text parser. It treats the whole document as
// a single section. A smarter heuristic (blank-line splitting, leading
// ALL-CAPS headings) can come later.
type Text struct{}

// NewText returns a new plain-text parser.
func NewText() *Text { return &Text{} }

// Name implements Parser.
func (*Text) Name() string { return "text" }

// Accepts implements Parser.
func (*Text) Accepts(contentType, filename string) bool {
	if contentType == "text/plain" {
		return true
	}
	return HasExt(filename, ".txt")
}

// Parse implements Parser.
func (*Text) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	body := string(b)

	title := firstNonEmptyLine(body)
	if len(title) > 120 {
		title = title[:120]
	}

	return &ParsedDoc{
		Title: title,
		Sections: []Section{{
			Level:   1,
			Title:   title,
			Content: strings.TrimSpace(body),
		}},
	}, nil
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
