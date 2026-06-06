package retrieval

import (
	"context"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// AutoStrategy is the default Vectorless strategy: it routes each query to
// the cheapest strategy that fits the document.
//
// Small documents — those whose total leaf content fits comfortably in one
// context window — are answered with Small (single-pass): one fast LLM call
// that picks section IDs straight from the outline. Larger or more complex
// documents fall through to Large (treewalk): the agentic tree-navigation
// strategy that expands and reads sections as it goes.
//
// This keeps the common case (small docs) fast and cheap while preserving
// Vectorless's tree-navigation behaviour where it actually earns its extra
// latency.
type AutoStrategy struct {
	// Small handles documents at or below SinglePassMaxTokens. Conceptually
	// SinglePass.
	Small Strategy
	// Large handles documents above the threshold. Conceptually TreeWalk.
	Large Strategy
	// SinglePassMaxTokens is the total leaf-content token count at or below
	// which Small is chosen. Zero means "derive from the call budget"
	// (budget.Available()) — i.e. if the whole document could fit in one
	// context window, treat it as small.
	SinglePassMaxTokens int
}

// NewAuto constructs an AutoStrategy from a small-document strategy and a
// large-document strategy.
func NewAuto(small, large Strategy) *AutoStrategy {
	return &AutoStrategy{Small: small, Large: large}
}

// Compile-time interface checks.
var (
	_ Strategy     = (*AutoStrategy)(nil)
	_ CostStrategy = (*AutoStrategy)(nil)
)

// Name implements Strategy.
func (a *AutoStrategy) Name() string { return "auto" }

// pick chooses the sub-strategy for a tree given the call budget. It never
// returns nil as long as both Small and Large are set; if a chosen branch
// is nil it falls back to the other so a half-configured Auto still runs.
func (a *AutoStrategy) pick(t *tree.Tree, budget ContextBudget) Strategy {
	// TreeWalk (the Large branch) navigates by PAGE RANGE, so it only works
	// when the document carries real page metadata. Non-paged formats
	// (Markdown / HTML / DOCX / TXT) leave every PageStart/PageEnd at 0, so
	// routing them to TreeWalk produces a page-navigation loop over a document
	// with no pages. Force the Small (outline-based) branch in that case.
	if !hasPageMetadata(t) {
		if a.Small != nil {
			return a.Small
		}
		return a.Large
	}

	threshold := a.SinglePassMaxTokens
	if threshold <= 0 {
		threshold = budget.Available()
	}
	small := threshold > 0 && sumLeafTokens(t) <= threshold
	if small {
		if a.Small != nil {
			return a.Small
		}
		return a.Large
	}
	if a.Large != nil {
		return a.Large
	}
	return a.Small
}

// Select implements Strategy.
func (a *AutoStrategy) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	s := a.pick(t, budget)
	if s == nil {
		// Misconfigured Auto (both Small and Large nil): an empty selection
		// is safer than a nil-pointer panic.
		return nil, nil
	}
	return s.Select(ctx, t, query, budget)
}

// SelectWithCost implements CostStrategy. It delegates to the chosen
// sub-strategy's CostStrategy when available so token usage + cost flow
// through, and falls back to Select otherwise.
func (a *AutoStrategy) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	s := a.pick(t, budget)
	if s == nil {
		return &Result{}, nil
	}
	if cs, ok := s.(CostStrategy); ok {
		return cs.SelectWithCost(ctx, t, query, budget)
	}
	ids, err := s.Select(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return &Result{SelectedIDs: ids}, nil
}

// sumLeafTokens sums the approximate token counts of every leaf section in
// the tree — a proxy for how much real content a query would have to reason
// over. Internal nodes are skipped to avoid double-counting their children.
func sumLeafTokens(t *tree.Tree) int {
	if t == nil {
		return 0
	}
	view := t.BuildView()
	total := 0
	for _, sv := range view.Sections {
		if len(sv.Children) == 0 {
			total += sv.Tokens
		}
	}
	return total
}

// hasPageMetadata reports whether any section in the tree carries a non-zero
// page range. TreeWalk navigation is only meaningful for paged documents;
// non-paged formats (Markdown / HTML / DOCX / TXT) leave PageStart/PageEnd at
// 0, so Auto must not route them to TreeWalk.
func hasPageMetadata(t *tree.Tree) bool {
	if t == nil {
		return false
	}
	for _, sv := range t.BuildView().Sections {
		if sv.PageStart > 0 || sv.PageEnd > 0 {
			return true
		}
	}
	return false
}
