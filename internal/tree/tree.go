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

	// ContentRef points to the full text of this section in storage.
	// Only leaf sections have content; internal sections summarize their
	// children.
	ContentRef string `json:"content_ref,omitempty"`

	// TokenCount is the approximate token count of the full content referenced
	// by ContentRef. Used for context budgeting during retrieval.
	TokenCount int `json:"token_count,omitempty"`

	// Metadata holds structural hints that retrieval strategies may use
	// (page ranges, keywords, entities, content type, etc.).
	Metadata map[string]string `json:"metadata,omitempty"`

	Children []*Section `json:"children,omitempty"`
}

// IsLeaf reports whether this section has no children.
func (s *Section) IsLeaf() bool {
	return len(s.Children) == 0
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
	Tokens   int         `json:"tokens,omitempty"`
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
			ID:       s.ID,
			ParentID: s.ParentID,
			Depth:    depth,
			Title:    s.Title,
			Summary:  s.Summary,
			Tokens:   s.TokenCount,
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
