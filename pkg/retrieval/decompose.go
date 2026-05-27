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
//
// This method does NOT surface per-pick confidences. Callers that need
// them should use DecomposedSelectWithConfidences (added in Phase 2.4).
func (d *Decomposer) DecomposedSelect(ctx context.Context, t *tree.Tree, plan *Plan, query string, budget ContextBudget) ([]tree.SectionID, Usage, error) {
	ids, _, usage, err := d.DecomposedSelectWithConfidences(ctx, t, plan, query, budget)
	return ids, usage, err
}

// DecomposedSelectWithConfidences is the Phase 2.4 variant of
// DecomposedSelect that also returns the per-pick confidence map.
// When a sub-question's underlying Strategy is a CostStrategy and
// surfaces confidences, those are unioned across sub-questions (max
// wins on duplicate IDs — the most confident sub-question wins).
//
// The returned confidences map is nil when no sub-question contributed
// any confidence signal at all — preserving the "no confidence signal"
// distinction the API layer's abstention check depends on.
func (d *Decomposer) DecomposedSelectWithConfidences(ctx context.Context, t *tree.Tree, plan *Plan, query string, budget ContextBudget) ([]tree.SectionID, map[tree.SectionID]float64, Usage, error) {
	if d == nil || d.Strategy == nil {
		return nil, nil, Usage{}, fmt.Errorf("decomposer: no strategy configured")
	}

	// Fall-through: no plan or not multi-hop. Single retrieval call on
	// the original query, with usage + confidences extracted from
	// CostStrategy when available.
	if plan == nil || !plan.IsMultiHop || len(plan.SubQuestions) == 0 {
		return d.runOnce(ctx, t, query, budget)
	}

	// Multi-hop path. Issue one retrieval call per sub-question, in
	// order. Stable order preserves the planner's intent — the first
	// sub-question is usually the most important — and gives a
	// deterministic union ordering callers can rely on.
	var (
		totalUsage  Usage
		out         = make([]tree.SectionID, 0)
		seen        = make(map[tree.SectionID]struct{})
		confidences map[tree.SectionID]float64
	)
	for _, sub := range plan.SubQuestions {
		ids, subConfidences, usage, err := d.runOnce(ctx, t, sub, budget)
		totalUsage.Add(usage)
		if err != nil {
			return out, confidences, totalUsage, fmt.Errorf("decompose %q: %w", sub, err)
		}
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
		// Union with max-wins on overlap: if two sub-questions both
		// score the same section, the more confident verdict carries.
		if len(subConfidences) > 0 {
			if confidences == nil {
				confidences = make(map[tree.SectionID]float64, len(subConfidences))
			}
			for id, c := range subConfidences {
				if existing, ok := confidences[id]; !ok || c > existing {
					confidences[id] = c
				}
			}
		}
	}
	return out, confidences, totalUsage, nil
}

// runOnce delegates one retrieval call. Uses CostStrategy when the
// wrapped strategy implements it so per-sub-question usage and (since
// Phase 2.4) confidences flow into the aggregated total; otherwise
// falls back to plain Select with a zero Usage value and nil
// confidences.
func (d *Decomposer) runOnce(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, map[tree.SectionID]float64, Usage, error) {
	if cs, ok := d.Strategy.(CostStrategy); ok {
		res, err := cs.SelectWithCost(ctx, t, query, budget)
		if err != nil {
			return nil, nil, Usage{}, err
		}
		if res == nil {
			return nil, nil, Usage{}, nil
		}
		return res.SelectedIDs, res.Confidences, res.Usage, nil
	}
	ids, err := d.Strategy.Select(ctx, t, query, budget)
	if err != nil {
		return nil, nil, Usage{}, err
	}
	return ids, nil, Usage{}, nil
}
