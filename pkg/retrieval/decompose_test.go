package retrieval_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// scriptedStrategy is a Strategy that returns canned per-query selections.
// Each call captures the query it received so tests can assert that the
// decomposer routed sub-questions to the right strategy invocation.
//
// Implements CostStrategy so we can verify Usage aggregation flows back
// out of the decomposer end-to-end.
type scriptedStrategy struct {
	mu      sync.Mutex
	calls   []string
	picks   map[string][]tree.SectionID
	usage   retrieval.Usage // returned (and added per-call) when CostStrategy is used
	errFor  map[string]error
	noCost  bool // when true, the strategy hides its CostStrategy implementation
	counter int32
}

func (s *scriptedStrategy) Name() string { return "scripted" }

func (s *scriptedStrategy) Select(ctx context.Context, t *tree.Tree, query string, budget retrieval.ContextBudget) ([]tree.SectionID, error) {
	atomic.AddInt32(&s.counter, 1)
	s.mu.Lock()
	s.calls = append(s.calls, query)
	s.mu.Unlock()
	if err, ok := s.errFor[query]; ok {
		return nil, err
	}
	if ids, ok := s.picks[query]; ok {
		out := make([]tree.SectionID, len(ids))
		copy(out, ids)
		return out, nil
	}
	return nil, nil
}

// costStrategyAdapter exposes Select + SelectWithCost. We wrap the
// scriptedStrategy in this adapter when we want to exercise the
// CostStrategy code path; the `noCost` field on scriptedStrategy is
// used to suppress this when we want the fall-through to plain Select.
type costStrategyAdapter struct {
	*scriptedStrategy
}

func (c *costStrategyAdapter) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget retrieval.ContextBudget) (*retrieval.Result, error) {
	ids, err := c.Select(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}
	return &retrieval.Result{
		SelectedIDs: ids,
		Usage:       c.usage,
	}, nil
}

// asStrategy returns either the cost-aware adapter or the bare strategy
// depending on whether we want to test the CostStrategy branch.
func (s *scriptedStrategy) asStrategy() retrieval.Strategy {
	if s.noCost {
		return s
	}
	return &costStrategyAdapter{scriptedStrategy: s}
}

// --- tests ---

func TestDecomposerFallthroughOnNilPlan(t *testing.T) {
	t.Parallel()
	tr := buildTree()
	s := &scriptedStrategy{
		picks: map[string][]tree.SectionID{
			"original query": {"sec_a"},
		},
		usage: retrieval.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10, LLMCalls: 1},
	}
	d := retrieval.NewDecomposer(s.asStrategy())

	ids, usage, err := d.DecomposedSelect(context.Background(), tr, nil, "original query", retrieval.ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "sec_a" {
		t.Errorf("want [sec_a], got %v", ids)
	}
	if usage.LLMCalls != 1 {
		t.Errorf("LLMCalls = %d, want 1 (single fall-through call)", usage.LLMCalls)
	}
	if got := atomic.LoadInt32(&s.counter); got != 1 {
		t.Errorf("strategy called %d times, want 1", got)
	}
	if len(s.calls) != 1 || s.calls[0] != "original query" {
		t.Errorf("strategy got %v, want [original query]", s.calls)
	}
}

func TestDecomposerFallthroughOnNonMultiHopPlan(t *testing.T) {
	t.Parallel()
	tr := buildTree()
	s := &scriptedStrategy{
		picks: map[string][]tree.SectionID{
			"q": {"sec_b"},
		},
	}
	d := retrieval.NewDecomposer(s.asStrategy())

	plan := &retrieval.Plan{
		Intent:     "factual_lookup",
		IsMultiHop: false,
	}
	ids, _, err := d.DecomposedSelect(context.Background(), tr, plan, "q", retrieval.ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "sec_b" {
		t.Errorf("want [sec_b], got %v", ids)
	}
	if got := atomic.LoadInt32(&s.counter); got != 1 {
		t.Errorf("strategy called %d times, want 1 (fall-through to original query)", got)
	}
}

func TestDecomposerFallthroughOnEmptySubQuestions(t *testing.T) {
	t.Parallel()
	tr := buildTree()
	s := &scriptedStrategy{
		picks: map[string][]tree.SectionID{
			"compound query": {"sec_c"},
		},
	}
	d := retrieval.NewDecomposer(s.asStrategy())

	// Pathological plan: IsMultiHop=true but no sub-questions. The
	// decomposer's fall-through guards against this directly even
	// though ParsePlan would have corrected it.
	plan := &retrieval.Plan{
		IsMultiHop:   true,
		SubQuestions: nil,
	}
	ids, _, err := d.DecomposedSelect(context.Background(), tr, plan, "compound query", retrieval.ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "sec_c" {
		t.Errorf("want [sec_c], got %v", ids)
	}
}

func TestDecomposerPerSubQuestionDispatch(t *testing.T) {
	t.Parallel()
	tr := buildTree()
	s := &scriptedStrategy{
		picks: map[string][]tree.SectionID{
			"What is the setup?": {"sec_a"},
			"What is the usage?": {"sec_b"},
			"What's in the FAQ?": {"sec_c"},
		},
		usage: retrieval.Usage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14, LLMCalls: 1, CostUSD: 0.001},
	}
	d := retrieval.NewDecomposer(s.asStrategy())

	plan := &retrieval.Plan{
		Intent:     "comparison",
		IsMultiHop: true,
		SubQuestions: []string{
			"What is the setup?",
			"What is the usage?",
			"What's in the FAQ?",
		},
	}
	ids, usage, err := d.DecomposedSelect(context.Background(), tr, plan, "compound original", retrieval.ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}

	// Per-sub-question dispatch: 3 strategy calls.
	if got := atomic.LoadInt32(&s.counter); got != 3 {
		t.Errorf("strategy called %d times, want 3 (one per sub-question)", got)
	}
	s.mu.Lock()
	calls := append([]string(nil), s.calls...)
	s.mu.Unlock()
	wantCalls := []string{
		"What is the setup?",
		"What is the usage?",
		"What's in the FAQ?",
	}
	for i, c := range wantCalls {
		if i >= len(calls) || calls[i] != c {
			t.Errorf("call[%d] = %q, want %q", i, calls[i], c)
		}
	}

	// Union ordering: first-seen across sub-questions.
	if len(ids) != 3 {
		t.Fatalf("want 3 unioned ids, got %v", ids)
	}
	if ids[0] != "sec_a" || ids[1] != "sec_b" || ids[2] != "sec_c" {
		t.Errorf("union order = %v, want [sec_a sec_b sec_c]", ids)
	}

	// Usage aggregation: 3 sub-questions × per-call usage.
	if usage.LLMCalls != 3 {
		t.Errorf("aggregated LLMCalls = %d, want 3", usage.LLMCalls)
	}
	if usage.TotalTokens != 42 {
		t.Errorf("aggregated TotalTokens = %d, want 42 (3 × 14)", usage.TotalTokens)
	}
	if usage.CostUSD < 0.003-1e-9 || usage.CostUSD > 0.003+1e-9 {
		t.Errorf("aggregated CostUSD = %v, want ~0.003", usage.CostUSD)
	}
}

// When sub-questions select overlapping section IDs the union must
// preserve first-seen order and drop duplicates.
func TestDecomposerUnionDedup(t *testing.T) {
	t.Parallel()
	tr := buildTree()
	s := &scriptedStrategy{
		picks: map[string][]tree.SectionID{
			"sub1": {"sec_a", "sec_b"},
			"sub2": {"sec_b", "sec_c"}, // sec_b overlaps with sub1
			"sub3": {"sec_a"},          // sec_a overlaps with sub1
		},
	}
	d := retrieval.NewDecomposer(s.asStrategy())

	plan := &retrieval.Plan{
		IsMultiHop:   true,
		SubQuestions: []string{"sub1", "sub2", "sub3"},
	}
	ids, _, err := d.DecomposedSelect(context.Background(), tr, plan, "q", retrieval.ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("want 3 unique ids, got %v", ids)
	}
	// Expected order: sec_a (from sub1), sec_b (from sub1), sec_c (from sub2).
	// sub2's sec_b and sub3's sec_a are skipped as duplicates.
	wantOrder := []tree.SectionID{"sec_a", "sec_b", "sec_c"}
	for i, want := range wantOrder {
		if ids[i] != want {
			t.Errorf("ids[%d] = %q, want %q (stable first-seen order)", i, ids[i], want)
		}
	}
}

// On a sub-question failure, the decomposer aborts and returns the
// partial Usage. The caller surfaces the error to its own /v1/query
// 500 — silently swallowing it would hide retrieval-side bugs.
func TestDecomposerErrorShortCircuits(t *testing.T) {
	t.Parallel()
	tr := buildTree()
	boom := errors.New("strategy boom")
	s := &scriptedStrategy{
		picks: map[string][]tree.SectionID{
			"sub1": {"sec_a"},
		},
		errFor: map[string]error{
			"sub2": boom,
		},
		usage: retrieval.Usage{LLMCalls: 1, TotalTokens: 5},
	}
	d := retrieval.NewDecomposer(s.asStrategy())

	plan := &retrieval.Plan{
		IsMultiHop:   true,
		SubQuestions: []string{"sub1", "sub2", "sub3"},
	}
	_, usage, err := d.DecomposedSelect(context.Background(), tr, plan, "q", retrieval.ContextBudget{})
	if err == nil {
		t.Fatal("expected error from sub2 failure, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v should wrap the strategy boom", err)
	}
	if usage.LLMCalls != 1 {
		t.Errorf("partial usage LLMCalls = %d, want 1 (sub1 only counted)", usage.LLMCalls)
	}
	// Strategy should NOT have been called on sub3 — we short-circuit.
	if got := atomic.LoadInt32(&s.counter); got != 2 {
		t.Errorf("strategy called %d times, want 2 (sub1 ok + sub2 err)", got)
	}
}

// When the wrapped strategy is NOT a CostStrategy, the decomposer still
// works — Usage is zero (we have nothing to aggregate) but selection
// behaviour is identical.
func TestDecomposerNonCostStrategy(t *testing.T) {
	t.Parallel()
	tr := buildTree()
	s := &scriptedStrategy{
		picks: map[string][]tree.SectionID{
			"sub1": {"sec_a"},
			"sub2": {"sec_b"},
		},
		noCost: true,
	}
	d := retrieval.NewDecomposer(s.asStrategy())

	plan := &retrieval.Plan{
		IsMultiHop:   true,
		SubQuestions: []string{"sub1", "sub2"},
	}
	ids, usage, err := d.DecomposedSelect(context.Background(), tr, plan, "q", retrieval.ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "sec_a" || ids[1] != "sec_b" {
		t.Errorf("want [sec_a sec_b], got %v", ids)
	}
	if usage.LLMCalls != 0 {
		t.Errorf("non-CostStrategy should report zero Usage, got %+v", usage)
	}
}

// Defensive: a nil decomposer is a programming bug. The error should be
// clear, not a nil panic.
func TestDecomposerNilStrategyErrors(t *testing.T) {
	t.Parallel()
	d := &retrieval.Decomposer{} // Strategy is nil
	_, _, err := d.DecomposedSelect(context.Background(), buildTree(), nil, "q", retrieval.ContextBudget{})
	if err == nil {
		t.Fatal("expected error when Strategy is nil")
	}
}

// End-to-end: a Planner + Decomposer chain with the mockLLM that drives
// the real SinglePass strategy. Verifies the wiring works as advertised
// in the task spec.
func TestPlannerPlusDecomposerEndToEnd(t *testing.T) {
	t.Parallel()
	tr := buildTree()

	// 1. Planner returns a multi-hop plan.
	pm := &plannerMock{
		replies: []string{
			jsonPlan(retrieval.Plan{
				Intent:       "comparison",
				IsMultiHop:   true,
				SubQuestions: []string{"sec_a please", "sec_c please"},
			}),
		},
	}
	planner := retrieval.NewPlanner(pm, "planner-model")
	plan, _, err := planner.Plan(context.Background(), "compare setup and faq")
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || !plan.IsMultiHop {
		t.Fatalf("expected multi-hop plan, got %+v", plan)
	}

	// 2. Decomposer wraps a real single-pass strategy with a mock LLM
	//    that picks by string presence — each sub-question contains the
	//    target section ID literal so picks are deterministic.
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a", "sec_c"}}
	s := retrieval.NewSinglePass(m)
	d := retrieval.NewDecomposer(s)

	ids, usage, err := d.DecomposedSelect(context.Background(), tr, plan, "compare setup and faq", retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("end-to-end union = %v, want 2 ids", ids)
	}
	// First sub-question fires first → sec_a comes before sec_c.
	if ids[0] != "sec_a" || ids[1] != "sec_c" {
		t.Errorf("union order = %v, want [sec_a sec_c]", ids)
	}
	if usage.LLMCalls != 2 {
		t.Errorf("usage LLMCalls = %d, want 2 (one per sub-question)", usage.LLMCalls)
	}
}
