// Package parser converts raw document bytes into a ParsedDoc — a
// hierarchical outline of sections that the ingest pipeline then turns
// into a tree.Tree.
//
// Each Parser is responsible for one input format (Markdown, HTML, PDF,
// DOCX, TXT, …). A Registry routes incoming bytes to the right parser
// based on content-type or filename extension.
//
// Parsers MUST NOT do any LLM work — they extract structure only.
// Summaries and embeddings are downstream concerns.
package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// ErrUnsupported is returned when no parser accepts the given input.
var ErrUnsupported = errors.New("parser: no parser for content type")

// ParsedDoc is the language-agnostic output of a parser.
type ParsedDoc struct {
	// Title is the document's top-level title, if known.
	Title string

	// Sections is the hierarchical outline of the document. Nesting is
	// expressed via Section.Children.
	Sections []Section

	// Metadata holds whatever extra structural hints the parser recovered
	// (author, created date, page count, etc.).
	Metadata map[string]string
}

// Section is one node in the parsed outline.
type Section struct {
	// Level is 1 for a top-level heading, 2 for a sub-heading, etc.
	// Parsers that can't recover a level may use 1 for every section.
	Level int

	// Title is the human-readable heading.
	Title string

	// Content is the full text of this section — not including children's
	// content. Empty for purely structural nodes.
	Content string

	// Children are nested sub-sections.
	Children []Section

	// Metadata is an optional map of structural hints for this section
	// (page range, heading anchor, etc.).
	Metadata map[string]string
}

// Parser is the format-specific contract.
type Parser interface {
	// Name is a short identifier ("markdown", "pdf", …).
	Name() string

	// Accepts returns true if this parser can handle the given
	// content-type (MIME) or filename.
	Accepts(contentType, filename string) bool

	// Parse reads r until EOF and returns the parsed outline.
	Parse(ctx context.Context, r io.Reader) (*ParsedDoc, error)
}

// Registry picks the right parser for a given input.
type Registry struct {
	parsers []Parser
}

// NewRegistry returns a Registry preloaded with ps.
func NewRegistry(ps ...Parser) *Registry {
	return &Registry{parsers: ps}
}

// Register adds a parser to the end of the match list.
func (r *Registry) Register(p Parser) {
	r.parsers = append(r.parsers, p)
}

// For returns the first parser that accepts the given input, or nil.
func (r *Registry) For(contentType, filename string) Parser {
	ct := normalizeContentType(contentType)
	fn := strings.ToLower(filename)
	for _, p := range r.parsers {
		if p.Accepts(ct, fn) {
			return p
		}
	}
	return nil
}

// Parse picks a parser via For and runs it. Returns ErrUnsupported if
// no parser matches.
func (r *Registry) Parse(ctx context.Context, contentType, filename string, body io.Reader) (*ParsedDoc, error) {
	p := r.For(contentType, filename)
	if p == nil {
		return nil, fmt.Errorf("%w: %q (%s)", ErrUnsupported, filename, contentType)
	}
	return p.Parse(ctx, body)
}

func normalizeContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct
}

// HasExt returns true if filename ends with any of the given extensions
// (leading dot optional).
func HasExt(filename string, exts ...string) bool {
	e := strings.ToLower(filepath.Ext(filename))
	for _, want := range exts {
		if !strings.HasPrefix(want, ".") {
			want = "." + want
		}
		if e == strings.ToLower(want) {
			return true
		}
	}
	return false
}

// Flatten returns the sections of d in depth-first, pre-order.
func (d *ParsedDoc) Flatten() []Section {
	var out []Section
	var walk func(secs []Section)
	walk = func(secs []Section) {
		for _, s := range secs {
			out = append(out, s)
			walk(s.Children)
		}
	}
	walk(d.Sections)
	return out
}
