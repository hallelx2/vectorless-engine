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

// treeWithLeafTokens builds a PAGED root with two leaf children whose token
// counts sum to total. Pages are set so Auto exercises the size-based routing
// (TreeWalk only applies to paged documents).
func treeWithLeafTokens(total int) *tree.Tree {
	half := total / 2
	return &tree.Tree{
		Root: &tree.Section{
			ID:    "root",
			Title: "Doc",
			Children: []*tree.Section{
				{ID: "a", ParentID: "root", Title: "A", TokenCount: half, ContentRef: "a.txt", PageStart: 1, PageEnd: 1},
				{ID: "b", ParentID: "root", Title: "B", TokenCount: total - half, ContentRef: "b.txt", PageStart: 2, PageEnd: 2},
			},
		},
	}
}

// treeNoPages builds the same shape but with NO page metadata (every
// PageStart/PageEnd is 0) — i.e. a non-paged format (Markdown/HTML/DOCX/TXT).
func treeNoPages(total int) *tree.Tree {
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

// A large but NON-PAGED document must NOT route to TreeWalk (which navigates
// by page range) — it falls back to Small.
func TestAutoStrategyNonPagedFallsBackToSmall(t *testing.T) {
	var called string
	small := recordingStrategy{name: "small", called: &called}
	large := recordingStrategy{name: "large", called: &called}
	auto := NewAuto(small, large)
	auto.SinglePassMaxTokens = 100

	called = ""
	// 1000 tokens (>> threshold) but no page metadata → must pick small.
	if _, err := auto.SelectWithCost(context.Background(), treeNoPages(1000), "q", ContextBudget{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != "small" {
		t.Fatalf("non-paged large doc routed to %q, want small", called)
	}
}

// A misconfigured Auto (both sub-strategies nil) must not panic.
func TestAutoStrategyNilSafe(t *testing.T) {
	auto := NewAuto(nil, nil)
	ids, err := auto.Select(context.Background(), treeWithLeafTokens(10), "q", ContextBudget{})
	if err != nil || ids != nil {
		t.Fatalf("Select on nil Auto: got (%v, %v), want (nil, nil)", ids, err)
	}
	res, err := auto.SelectWithCost(context.Background(), treeWithLeafTokens(10), "q", ContextBudget{})
	if err != nil || res == nil || len(res.SelectedIDs) != 0 {
		t.Fatalf("SelectWithCost on nil Auto: got (%v, %v), want empty Result", res, err)
	}
}
