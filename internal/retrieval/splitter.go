package retrieval

import (
	"context"

	"github.com/hallelx2/vectorless-engine/internal/llm"
	"github.com/hallelx2/vectorless-engine/internal/tree"
)

// Slice is a portion of a tree view small enough to fit in one LLM call.
//
// Every Slice carries a Breadcrumb that tells the model where in the full
// document this slice lives — essential when the full tree has been split
// across multiple parallel reasoning calls.
type Slice struct {
	// Breadcrumb is a short, human-readable path into the document, e.g.
	//   "Document: Annual Report 2025 → Part II → (sections II.3–II.7 of 12)"
	// The model uses this to orient itself relative to unseen siblings.
	Breadcrumb string

	// Sections is the compact section view included in this slice.
	Sections []tree.SectionView

	// SiblingSummaries optionally includes short summaries of sibling
	// subtrees not fully expanded in this slice — useful as "peripheral
	// vision" for the model's relevance judgments.
	SiblingSummaries []tree.SectionView
}

// Splitter breaks a Tree view into slices that each fit a ContextBudget.
type Splitter interface {
	Split(ctx context.Context, t *tree.Tree, budget ContextBudget, tokenizer Tokenizer) ([]Slice, error)
}

// Tokenizer counts tokens for a given string under the target model.
//
// Strategies accept an llm.Client and wrap it in a Tokenizer so the splitter
// does not need to know about LLM providers directly.
type Tokenizer interface {
	Count(ctx context.Context, text string) (int, error)
}

// LLMTokenizer adapts an llm.Client to the Tokenizer interface.
type LLMTokenizer struct{ C llm.Client }

func (t LLMTokenizer) Count(ctx context.Context, s string) (int, error) {
	return t.C.CountTokens(ctx, s)
}

// DefaultSplitter is a structure-aware splitter that respects the tree's
// hierarchy. It tries to keep sibling groups together and preserves a
// breadcrumb path into each slice so the model has orientation.
//
// Algorithm (high level):
//
//  1. If the full tree view fits the budget, return one slice.
//  2. Otherwise, walk children of the root. Greedily bin-pack children into
//     slices that fit the budget. Each child's subtree is fully included.
//  3. If a single child's subtree is itself larger than the budget, recurse
//     into that subtree with the same algorithm.
//
// This yields slices that respect semantic boundaries (a section is never
// cut mid-subtree) while staying within the model's context window.
type DefaultSplitter struct{}

// NewDefaultSplitter returns the structure-aware splitter used by the
// chunked-tree strategy.
func NewDefaultSplitter() *DefaultSplitter { return &DefaultSplitter{} }

// Split implements Splitter.
//
// Note: this is the scaffold skeleton. The bin-packing + recursion logic is
// straightforward and will be filled in during Phase 1 alongside the ingest
// pipeline that produces the Summary fields the splitter measures.
func (d *DefaultSplitter) Split(ctx context.Context, t *tree.Tree, budget ContextBudget, tok Tokenizer) ([]Slice, error) {
	// TODO(phase-1): implement the algorithm described above. For the
	// scaffold we return a single slice containing the full flattened view,
	// matching the single-pass strategy's behavior. This keeps the engine
	// functional end-to-end while deferring the bin-packing work.
	view := t.BuildView()
	return []Slice{{
		Breadcrumb: "Document: " + view.Title,
		Sections:   view.Sections,
	}}, nil
}
