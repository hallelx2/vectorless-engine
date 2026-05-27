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

	"github.com/hallelx2/vectorless-engine/pkg/tree"
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
// reasoning trace and cost accounting when the strategy supports it.
type Result struct {
	SelectedIDs []tree.SectionID `json:"selected_ids"`
	Reasoning   string           `json:"reasoning,omitempty"`
	ModelUsed   string           `json:"model_used,omitempty"`
	Usage       Usage            `json:"usage"`

	// HopsTaken is the number of LLM turns the strategy issued to reach the
	// final selection. Single-shot strategies set this to 1; iterative
	// strategies (e.g. agentic) set it to the number of tool-using turns
	// actually consumed, including the terminal "done" turn.
	HopsTaken int `json:"hops_taken,omitempty"`

	// TraceToken is the replay token computed by ComputeTraceToken over
	// the inputs that determine selection (document ID + version,
	// retrieval model, system prompt version, sorted selected IDs).
	// Two retrieval runs with identical inputs produce the same token,
	// regardless of reasoning path. Empty when the strategy did not
	// populate it (e.g. tests, fallback paths).
	TraceToken string `json:"trace_token,omitempty"`
}

// Usage is the aggregated token + cost accounting across all LLM calls
// made during a single retrieval run.
type Usage struct {
	// InputTokens is the total prompt tokens across all calls.
	InputTokens int
	// OutputTokens is the total completion tokens across all calls.
	OutputTokens int
	// TotalTokens is InputTokens + OutputTokens.
	TotalTokens int
	// CostUSD is the estimated cost in US dollars. 0 if unknown.
	CostUSD float64
	// LLMCalls is the number of LLM calls made.
	LLMCalls int
}

// Add adds the token counts and cost from another Usage.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens += other.TotalTokens
	u.CostUSD += other.CostUSD
	u.LLMCalls += other.LLMCalls
}

// CostStrategy extends Strategy with a richer return value that
// includes cost accounting. Strategies that support cost tracking
// implement this interface in addition to Strategy. The server
// checks for it at runtime via type assertion.
type CostStrategy interface {
	Strategy

	// SelectWithCost works like Select but returns a full Result
	// including token usage and cost.
	SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error)
}
