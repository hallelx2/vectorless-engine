package retrieval

import (
	"context"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// recordingStrategy records its name into *called when invoked so a test
// can assert which branch AutoStrategy routed to.
type recordingStrategy struct {
	name   string
	called *string
}

func (r recordingStrategy) Name() string { return r.name }

func (r recordingStrategy) Select(_ context.Context, _ *tree.Tree, _ string, _ ContextBudget) ([]tree.SectionID, error) {
	*r.called = r.name
	return []tree.SectionID{"sec"}, nil
}

func (r recordingStrategy) SelectWithCost(_ context.Context, _ *tree.Tree, _ string, _ ContextBudget) (*Result, error) {
	*r.called = r.name
	return &Result{SelectedIDs: []tree.SectionID{"sec"}}, nil
}

// treeWithLeafTokens builds a root with two leaf children whose token
// counts sum to total.
func treeWithLeafTokens(total int) *tree.Tree {
	half := total / 2
	return &tree.Tree{
		Root: &tree.Section{
			ID:    "root",
			Title: "Doc",
			Children: []*tree.Section{
				{ID: "a", ParentID: "root", Title: "A", TokenCount: half, ContentRef: "a.txt"},
				{ID: "b", ParentID: "root", Title: "B", TokenCount: total - half, ContentRef: "b.txt"},
			},
		},
	}
}

func TestAutoStrategyRoutesByExplicitThreshold(t *testing.T) {
	var called string
	small := recordingStrategy{name: "small", called: &called}
	large := recordingStrategy{name: "large", called: &called}
	auto := NewAuto(small, large)
	auto.SinglePassMaxTokens = 100

	// Below threshold → small.
	called = ""
	if _, err := auto.SelectWithCost(context.Background(), treeWithLeafTokens(40), "q", ContextBudget{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != "small" {
		t.Fatalf("small doc routed to %q, want small", called)
	}

	// Above threshold → large.
	called = ""
	if _, err := auto.SelectWithCost(context.Background(), treeWithLeafTokens(1000), "q", ContextBudget{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != "large" {
		t.Fatalf("large doc routed to %q, want large", called)
	}
}

func TestAutoStrategyDerivesThresholdFromBudget(t *testing.T) {
	var called string
	small := recordingStrategy{name: "small", called: &called}
	large := recordingStrategy{name: "large", called: &called}
	auto := NewAuto(small, large) // SinglePassMaxTokens == 0 → use budget.Available()

	budget := ContextBudget{MaxTokens: 1000, ReservedForPrompt: 200} // Available()==800

	called = ""
	if _, err := auto.Select(context.Background(), treeWithLeafTokens(500), "q", budget); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != "small" {
		t.Fatalf("doc within budget routed to %q, want small", called)
	}

	called = ""
	if _, err := auto.Select(context.Background(), treeWithLeafTokens(2000), "q", budget); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != "large" {
		t.Fatalf("doc exceeding budget routed to %q, want large", called)
	}
}

func TestAutoStrategyName(t *testing.T) {
	if got := NewAuto(nil, nil).Name(); got != "auto" {
		t.Fatalf("Name()=%q, want auto", got)
	}
}
