package retrieval

import (
	"context"

	"github.com/hallelx2/vectorless-engine/internal/llm"
	"github.com/hallelx2/vectorless-engine/internal/tree"
)

// SinglePass is the simplest Strategy: feed the entire tree view to the model
// in one call and ask it to pick relevant section IDs.
//
// Use when the tree fits comfortably in the model's context window. For
// larger documents, ChunkedTree is the right choice.
type SinglePass struct {
	LLM llm.Client
}

// NewSinglePass constructs a SinglePass strategy backed by client.
func NewSinglePass(client llm.Client) *SinglePass {
	return &SinglePass{LLM: client}
}

// Name implements Strategy.
func (s *SinglePass) Name() string { return "single-pass" }

// Select implements Strategy.
//
// The scaffold returns an empty result without calling the LLM. Real
// reasoning is added alongside the LLM client implementations in Phase 1.
func (s *SinglePass) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	// TODO(phase-1):
	//   1. Build a compact prompt: system rules + tree view JSON + user query.
	//   2. Call s.LLM.Complete with JSONMode + schema constraining output to
	//      { "selected_section_ids": ["..."], "reasoning": "..." }.
	//   3. Validate every returned ID exists in t; drop unknowns.
	//   4. Return the deduplicated ID list.
	return nil, nil
}
