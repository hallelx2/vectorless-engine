// Package tree is the core data model of the vectorless engine.
//
// A Tree is a hierarchical outline of a document — titles, summaries, and
// metadata for every section. Leaves reference full section content stored in
// a Storage backend; they are not inlined in the tree.
//
// The tree is the retrieval index. There are no chunks and no embeddings.
// At query time, a retrieval strategy reasons over the tree to decide which
// SectionIDs should be fetched in full.
package tree

import (
	"time"
)

// SectionID uniquely identifies a section within a document.
type SectionID string

// DocumentID uniquely identifies a document.
type DocumentID string

// Tree is the structured outline of a single document.
//
// A Tree is cheap to serialize and small relative to the document itself:
// typically a few KB of titles + short summaries even for books. It is
// designed to fit comfortably in an LLM context window for reasoning.
type Tree struct {
	DocumentID DocumentID `json:"document_id"`
	Title      string     `json:"title"`
	Root       *Section   `json:"root"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Section is a node in the tree. Internal nodes have children; leaves have
// content stored in a Storage backend referenced by ContentRef.
type Section struct {
	ID       SectionID `json:"id"`
	ParentID SectionID `json:"parent_id,omitempty"`
	Ordinal  int       `json:"ordinal"`

	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`

	// SummaryAxes is the structured, multi-axis summary written by the
	// Phase 2.5 summarizer. Carries topics / entities / numbers / one_line.
	// nil for sections written before multi-axis summaries shipped (the
	// retrieval prompt simply skips the extra axes lines when nil).
	//
	// SummaryAxes.OneLine and the flat `Summary` field above are kept in
	// sync at write time so older callers that only read `summary` keep
	// working unchanged.
	SummaryAxes *SummaryAxes `json:"summary_axes,omitempty"`

	// ContentRef points to the full text of this section in storage.
	// Only leaf sections have content; internal sections summarize their
	// children.
	ContentRef string `json:"content_ref,omitempty"`

	// TokenCount is the approximate token count of the full content referenced
	// by ContentRef. Used for context budgeting during retrieval.
	TokenCount int `json:"token_count,omitempty"`

	// PageStart / PageEnd is the inclusive page range this section covers.
	// Zero (the default) means "unknown" — non-paginated formats (Markdown,
	// HTML, DOCX, text) leave both at 0; the PDF parser populates them.
	PageStart int `json:"page_start,omitempty"`
	PageEnd   int `json:"page_end,omitempty"`

	// CandidateQuestions is the HyDE-generated list of questions this
	// section can answer, written by the ingest pipeline. Empty for
	// sections that haven't been HyDE'd yet, internal nodes that skip
	// the stage, or when the LLM produces non-parseable output.
	CandidateQuestions []string `json:"candidate_questions,omitempty"`

	// Metadata holds structural hints that retrieval strategies may use
	// (page ranges, keywords, entities, content type, etc.).
	Metadata map[string]string `json:"metadata,omitempty"`

	Children []*Section `json:"children,omitempty"`
}

// SummaryAxes is the multi-axis structured summary the Phase 2.5
// summarizer produces. Each axis gives retrieval a different angle on
// the section's content:
//
//   - Topics:   hyphenated keywords ("debt", "long-term-obligations")
//     for coarse subject-matter matching.
//   - Entities: proper nouns extracted verbatim (orgs, people, places,
//     dates) so retrieval can match queries that mention them by name.
//   - Numbers:  standout numeric values with the units as they appear
//     in the section ("$4.2B", "2.8%") — preserves the surface form so
//     a downstream model doesn't have to re-normalise.
//   - OneLine:  the human-readable sentence describing the section.
//     Persisted into the plain `summary` field on Section so older
//     SDKs / API consumers that read `summary` continue to work.
//
// All axes are optional; a parse failure or content with no extractable
// entities/numbers can leave individual fields empty without invalidating
// the rest of the object.
type SummaryAxes struct {
	Topics   []string `json:"topics,omitempty"`
	Entities []string `json:"entities,omitempty"`
	Numbers  []string `json:"numbers,omitempty"`
	OneLine  string   `json:"one_line,omitempty"`
}

// IsLeaf reports whether this section has no children.
func (s *Section) IsLeaf() bool {
	return len(s.Children) == 0
}

// TOCNode is one node in the LLM-built table-of-contents tree
// persisted on Document.toc_tree. Distinct from Section because
// it represents the document's logical outline (headings the LLM
// recovered or invented from body text) rather than the parser's
// chunked content tree. Used by the PageIndex-style retrieval
// strategy that reasons over the TOC before drilling into sections.
//
// Structure carries the PageIndex-style hierarchical index ("1",
// "1.1", "1.1.2"). Title is the original heading verbatim (spacing
// fixed). StartPage is 1-indexed and refers to the source PDF's
// physical page. EndPage is derived from the next sibling's
// StartPage at build time (when known); zero means "unknown / open"
// and downstream readers should treat the node as running until
// either the next sibling at the same depth or the document end.
//
// The shape mirrors PageIndex's tree-output JSON (start_page /
// end_page / nodes) so external tooling that expects that
// vocabulary can interop without translation.
type TOCNode struct {
	// NodeID is a stable identifier for this TOC node within its
	// owning document. Generated by the builder; opaque to clients.
	NodeID string `json:"node_id"`

	// Structure is the dotted hierarchical index ("1", "1.1",
	// "1.1.2"). Empty for roots that the builder couldn't number.
	Structure string `json:"structure"`

	// Title is the section's heading text. Always populated.
	Title string `json:"title"`

	// StartPage is the 1-indexed PDF page where this section
	// begins. The verification phase checks that the title
	// actually appears at the start of this page; mismatches are
	// repaired before persistence.
	StartPage int `json:"start_page"`

	// EndPage is the 1-indexed inclusive end page derived from
	// sibling ordering. Zero means "unknown / open" and should be
	// interpreted as running to the next sibling's StartPage - 1
	// (or document end).
	EndPage int `json:"end_page,omitempty"`

	// Summary is an optional one-line description of the
	// subsection's content. Populated only when the builder runs
	// with summary-generation enabled (a follow-up PR; left blank
	// here so the JSON shape is forward-compatible).
	Summary string `json:"summary,omitempty"`

	// Nodes is the recursive list of child TOC nodes in document
	// order.
	Nodes []TOCNode `json:"nodes,omitempty"`
}

// Walk visits every section in depth-first, pre-order. Traversal stops if
// visit returns false.
func (s *Section) Walk(visit func(*Section) bool) {
	if s == nil || !visit(s) {
		return
	}
	for _, c := range s.Children {
		c.Walk(visit)
	}
}

// FindByID returns the section with the given ID, or nil if not found.
func (t *Tree) FindByID(id SectionID) *Section {
	var out *Section
	t.Root.Walk(func(s *Section) bool {
		if s.ID == id {
			out = s
			return false
		}
		return true
	})
	return out
}

// View returns a compact representation of the tree for LLM consumption.
// Only titles, summaries, and IDs are included — not full content.
// This is what a retrieval strategy feeds to the model to reason over.
type View struct {
	DocumentID DocumentID    `json:"document_id"`
	Title      string        `json:"title"`
	Sections   []SectionView `json:"sections"`
}

// SectionView is a flattened, compact projection of a Section suitable for
// inclusion in a prompt. Children are represented by ID references.
type SectionView struct {
	ID       SectionID   `json:"id"`
	ParentID SectionID   `json:"parent_id,omitempty"`
	Depth    int         `json:"depth"`
	Title    string      `json:"title"`
	Summary  string      `json:"summary,omitempty"`
	Children []SectionID `json:"children,omitempty"`
	Tokens   int         `json:"tokens"`

	// PageStart / PageEnd mirror the Section fields so retrieval prompts
	// and API responses can cite page ranges. Zero means "unknown".
	PageStart int `json:"page_start,omitempty"`
	PageEnd   int `json:"page_end,omitempty"`

	// CandidateQuestions are the HyDE-generated questions this section
	// can answer. Surfaced into the retrieval prompt to widen the model's
	// lexical/semantic overlap with the user query.
	CandidateQuestions []string `json:"candidate_questions,omitempty"`

	// SummaryAxes mirrors Section.SummaryAxes so the retrieval prompt
	// can surface topics / entities / numbers alongside the one-line
	// summary. nil for pre-Phase-2.5 sections.
	SummaryAxes *SummaryAxes `json:"summary_axes,omitempty"`
}

// BuildView renders the tree as a flat list of SectionViews in depth-first
// order. This form is easy to trim, slice, and serialize into prompts.
func (t *Tree) BuildView() View {
	out := View{
		DocumentID: t.DocumentID,
		Title:      t.Title,
	}
	var walk func(s *Section, depth int)
	walk = func(s *Section, depth int) {
		if s == nil {
			return
		}
		sv := SectionView{
			ID:                 s.ID,
			ParentID:           s.ParentID,
			Depth:              depth,
			Title:              s.Title,
			Summary:            s.Summary,
			Tokens:             s.TokenCount,
			PageStart:          s.PageStart,
			PageEnd:            s.PageEnd,
			CandidateQuestions: s.CandidateQuestions,
			SummaryAxes:        s.SummaryAxes,
		}
		for _, c := range s.Children {
			sv.Children = append(sv.Children, c.ID)
		}
		out.Sections = append(out.Sections, sv)
		for _, c := range s.Children {
			walk(c, depth+1)
		}
	}
	walk(t.Root, 0)
	return out
}
