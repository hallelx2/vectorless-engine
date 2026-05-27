package retrieval

import (
	"context"
	"fmt"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Decomposer runs the wrapped Strategy once per sub-question in a
// multi-hop plan and unions the per-sub-question selections. Each
// sub-question gets its own focused retrieval call — the strategy sees
// one tight question instead of the compound original.
//
// Decomposer is deliberately strategy-agnostic: it composes on top of
// any Strategy implementation (single-pass, chunked-tree, agentic, even
// the cached wrapper). When the wrapped strategy is a CostStrategy the
// per-sub-question Usage is aggregated so /v1/answer's accounting stays
// honest.
//
// Fall-through:
//
//   - nil Plan, IsMultiHop false, or empty SubQuestions → delegate to
//     Strategy.Select with the original query unchanged. The decomposer
//     is transparent in this case; callers don't need to branch.
type Decomposer struct {
	Strategy Strategy
}

// NewDecomposer wraps s with the decomposition dispatcher.
func NewDecomposer(s Strategy) *Decomposer {
	return &Decomposer{Strategy: s}
}

// DecomposedSelect runs the strategy according to plan. When plan is
// nil, missing IsMultiHop, or has no SubQuestions, it falls through to
// Strategy.Select on the original query. Otherwise it runs Strategy once
// per sub-question and returns the union of selected IDs in stable
// first-seen order, plus aggregated Usage across all underlying calls.
//
// Errors short-circuit: the first sub-question failure aborts and
// returns the partial Usage gathered up to that point. This is the same
// failure contract Strategy.Select has — a multi-hop loop shouldn't
// silently mask retrieval errors.
func (d *Decomposer) DecomposedSelect(ctx context.Context, t *tree.Tree, plan *Plan, query string, budget ContextBudget) ([]tree.SectionID, Usage, error) {
	if d == nil || d.Strategy == nil {
		return nil, Usage{}, fmt.Errorf("decomposer: no strategy configured")
	}

	// Fall-through: no plan or not multi-hop. Single retrieval call on
	// the original query, with usage extracted from CostStrategy when
	// available.
	if plan == nil || !plan.IsMultiHop || len(plan.SubQuestions) == 0 {
		return d.runOnce(ctx, t, query, budget)
	}

	// Multi-hop path. Issue one retrieval call per sub-question, in
	// order. Stable order preserves the planner's intent — the first
	// sub-question is usually the most important — and gives a
	// deterministic union ordering callers can rely on.
	var (
		totalUsage Usage
		out        = make([]tree.SectionID, 0)
		seen       = make(map[tree.SectionID]struct{})
	)
	for _, sub := range plan.SubQuestions {
		ids, usage, err := d.runOnce(ctx, t, sub, budget)
		totalUsage.Add(usage)
		if err != nil {
			return out, totalUsage, fmt.Errorf("decompose %q: %w", sub, err)
		}
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out, totalUsage, nil
}

// runOnce delegates one retrieval call. Uses CostStrategy when the
// wrapped strategy implements it so per-sub-question usage flows into
// the aggregated total; otherwise falls back to plain Select with a
// zero Usage value.
func (d *Decomposer) runOnce(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, Usage, error) {
	if cs, ok := d.Strategy.(CostStrategy); ok {
		res, err := cs.SelectWithCost(ctx, t, query, budget)
		if err != nil {
			return nil, Usage{}, err
		}
		if res == nil {
			return nil, Usage{}, nil
		}
		return res.SelectedIDs, res.Usage, nil
	}
	ids, err := d.Strategy.Select(ctx, t, query, budget)
	if err != nil {
		return nil, Usage{}, err
	}
	return ids, Usage{}, nil
}
