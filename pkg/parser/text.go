package parser

import (
	"context"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// Text parses plain text. It splits the document into sections on lines
// that look like headings — ALL-CAPS lines, or numbered/labelled headings
// such as "1.2 Scope", "Chapter 3", "Section II", "Appendix A" — and
// treats everything else as body. A document with no recognisable headings
// degrades to a single section, but most real-world text has structure
// worth recovering for tree navigation.
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

// textHeadingRe matches common labelled/numbered heading prefixes.
var textHeadingRe = regexp.MustCompile(`(?i)^(\d+(\.\d+)*\.?\s+\S|(chapter|section|part|appendix|article)\s+([0-9]+|[ivxlcdm]+|[a-z])\b)`)

// Parse implements Parser.
func (*Text) Parse(_ context.Context, r io.Reader) (*ParsedDoc, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	body := string(b)

	lines := strings.Split(body, "\n")

	type acc struct {
		title string
		body  []string
	}
	flatsAcc := []*acc{{}} // preamble bucket (no title)
	current := flatsAcc[0]
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if looksLikeTextHeading(line) {
			current = &acc{title: strings.TrimSpace(line)}
			flatsAcc = append(flatsAcc, current)
			continue
		}
		current.body = append(current.body, line)
	}

	// Convert to flat sections: preamble is level 0, each heading is level 1.
	flats := make([]flatSection, 0, len(flatsAcc))
	for i, a := range flatsAcc {
		level := 1
		if i == 0 {
			level = 0
		}
		flats = append(flats, flatSection{
			Level:   level,
			Title:   a.title,
			Content: strings.TrimSpace(strings.Join(a.body, "\n")),
		})
	}
	flats = dropEmptyPreamble(flats)

	title := deriveTitle(flats)
	if title == "" {
		title = firstNonEmptyLine(body)
	}
	if len(title) > 120 {
		title = title[:120]
	}

	return &ParsedDoc{
		Title:    title,
		Sections: buildSections(flats),
	}, nil
}

// looksLikeTextHeading applies conservative heuristics to decide whether a
// line is a section heading rather than body text. It errs toward NOT
// splitting: a false heading fragments a section, which is worse than a
// missed one.
func looksLikeTextHeading(line string) bool {
	l := strings.TrimSpace(line)
	if l == "" || len(l) > 80 {
		return false
	}
	// A line ending in sentence punctuation is prose, not a heading.
	if strings.HasSuffix(l, ",") || strings.HasSuffix(l, ";") {
		return false
	}
	if textHeadingRe.MatchString(l) {
		return true
	}
	// ALL-CAPS line with at least two letters (and no terminal period).
	letters, upper := 0, 0
	for _, r := range l {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if letters >= 2 && upper == letters && !strings.HasSuffix(l, ".") {
		return true
	}
	return false
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
