// Package retrieval selects which sections of a document are relevant to a
// query by reasoning over the tree.
//
// The engine ships with two strategies:
//
//   - single-pass: small tree fits in one LLM call; the model picks section
//     IDs directly.
//   - chunked-tree: tree is split into context-budget-sized subtrees,
//     reasoned over in parallel, then results merged.
//
// Additional strategies are a simple interface away. Each strategy shares
// the same tree data model; only the reasoning loop differs.
package retrieval

import (
	"context"

	"github.com/hallelx2/vectorless-engine/internal/tree"
)

// Strategy is the contract every retrieval strategy satisfies.
type Strategy interface {
	// Name is the stable identifier used in configuration and telemetry.
	Name() string

	// Select returns the section IDs the model judged relevant to query.
	Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error)
}

// ContextBudget bounds a single LLM call.
//
// The engine uses it to decide when a tree is small enough for a single-pass
// strategy and, for chunked-tree, how aggressively to split.
type ContextBudget struct {
	// ModelName is the model that will be called. Informational; strategies
	// use it for logging and model-aware behavior.
	ModelName string

	// MaxTokens is the full context window of the model.
	MaxTokens int

	// ReservedForPrompt is the budget reserved for the system prompt,
	// user instructions, and response tokens — i.e. not available to the
	// tree view itself.
	ReservedForPrompt int

	// MaxParallelCalls bounds fan-out when chunking is required.
	MaxParallelCalls int
}

// Available computes how many tokens of tree view can fit in one call.
func (b ContextBudget) Available() int {
	v := b.MaxTokens - b.ReservedForPrompt
	if v < 0 {
		return 0
	}
	return v
}

// Result is returned to the API layer. It includes not just IDs but the
// reasoning trace when the strategy supports it.
type Result struct {
	SelectedIDs []tree.SectionID
	Reasoning   string
	ModelUsed   string
}
