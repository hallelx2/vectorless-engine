package retrieval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
)

// plannerMock is a minimal llmgate client that returns scripted replies
// in order. Each Complete() call advances the index; if calls exceed the
// scripted replies the last reply is returned (so a single-element script
// turns into a fixed response).
//
// Distinct from the retrieval_test.go mockLLM because the planner doesn't
// care about prompt-content-driven picks — it speaks to a different
// schema. Keeping the mocks separate avoids muddying mockLLM with
// planner-specific behaviour.
type plannerMock struct {
	mu      sync.Mutex
	replies []string
	err     error

	calls   int32
	prompts []string
}

func (m *plannerMock) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	atomic.AddInt32(&m.calls, 1)
	for _, msg := range req.Messages {
		if msg.Role == llmgate.RoleUser {
			m.prompts = append(m.prompts, msg.Content)
		}
	}
	if m.err != nil {
		return nil, m.err
	}
	if len(m.replies) == 0 {
		return &llmgate.Response{}, nil
	}
	idx := int(atomic.LoadInt32(&m.calls)) - 1
	if idx >= len(m.replies) {
		idx = len(m.replies) - 1
	}
	return &llmgate.Response{
		Content: m.replies[idx],
		Usage: llmgate.Usage{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
			CostUSD:      0.0001,
		},
	}, nil
}

func (m *plannerMock) CountTokens(ctx context.Context, s string) (int, error) {
	return len(s) / 4, nil
}

// jsonPlan marshals a Plan for use as a mock LLM reply.
func jsonPlan(p retrieval.Plan) string {
	raw, _ := json.Marshal(p)
	return string(raw)
}

func TestPlannerHappyPath(t *testing.T) {
	t.Parallel()
	m := &plannerMock{
		replies: []string{
			jsonPlan(retrieval.Plan{
				Intent:           "factual_lookup",
				Entities:         []string{"Acme Corp", "Q4 2024"},
				ExpectedDocAreas: []string{"income statement"},
				IsMultiHop:       false,
			}),
		},
	}
	p := retrieval.NewPlanner(m, "test-model")

	plan, usage, err := p.Plan(context.Background(), "What was Acme Corp's Q4 2024 revenue?")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan == nil {
		t.Fatal("expected a plan, got nil")
	}
	if plan.Intent != "factual_lookup" {
		t.Errorf("intent = %q, want factual_lookup", plan.Intent)
	}
	if plan.IsMultiHop {
		t.Error("IsMultiHop should be false")
	}
	if len(plan.Entities) != 2 {
		t.Errorf("entities len = %d, want 2 (%v)", len(plan.Entities), plan.Entities)
	}
	if usage.LLMCalls != 1 {
		t.Errorf("LLMCalls = %d, want 1", usage.LLMCalls)
	}
}

func TestPlannerCacheHitSkipsLLM(t *testing.T) {
	t.Parallel()
	m := &plannerMock{
		replies: []string{
			jsonPlan(retrieval.Plan{Intent: "summary", IsMultiHop: false}),
		},
	}
	p := retrieval.NewPlanner(m, "test-model")

	// First call → LLM hit.
	if _, _, err := p.Plan(context.Background(), "Summarise the document"); err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&m.calls); c != 1 {
		t.Fatalf("want 1 LLM call on first invocation, got %d", c)
	}
	// Second call same query → cache hit, no LLM call.
	plan, usage, err := p.Plan(context.Background(), "Summarise the document")
	if err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&m.calls); c != 1 {
		t.Fatalf("want still 1 LLM call (cache hit), got %d", c)
	}
	if plan == nil || plan.Intent != "summary" {
		t.Errorf("cached plan = %+v, want intent=summary", plan)
	}
	if usage.LLMCalls != 0 {
		t.Errorf("cached call LLMCalls = %d, want 0", usage.LLMCalls)
	}

	stats := p.CacheStats()
	if stats.Hits != 1 {
		t.Errorf("cache hits = %d, want 1", stats.Hits)
	}
}

// A different query bypasses the cache and issues another LLM call.
func TestPlannerCacheMissOnDifferentQuery(t *testing.T) {
	t.Parallel()
	m := &plannerMock{
		replies: []string{
			jsonPlan(retrieval.Plan{Intent: "a", IsMultiHop: false}),
			jsonPlan(retrieval.Plan{Intent: "b", IsMultiHop: false}),
		},
	}
	p := retrieval.NewPlanner(m, "test-model")
	_, _, _ = p.Plan(context.Background(), "query A")
	_, _, _ = p.Plan(context.Background(), "query B")
	if c := atomic.LoadInt32(&m.calls); c != 2 {
		t.Errorf("two distinct queries should hit LLM twice, got %d", c)
	}
}

// Two concurrent identical queries — the cache must dedup so we don't
// double-spend on the same prompt.
func TestPlannerConcurrentSameQuery(t *testing.T) {
	t.Parallel()
	m := &plannerMock{
		replies: []string{
			jsonPlan(retrieval.Plan{Intent: "concurrent", IsMultiHop: false}),
		},
	}
	p := retrieval.NewPlanner(m, "test-model")

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = p.Plan(context.Background(), "race query")
		}()
	}
	wg.Wait()
	if c := atomic.LoadInt32(&m.calls); c != 1 {
		t.Errorf("concurrent identical queries should fold to 1 LLM call, got %d", c)
	}
}

// On a non-JSON reply the planner must retry, then degrade gracefully to
// a nil plan + nil error so the caller can continue without planning.
func TestPlannerRetryOnBadJSON(t *testing.T) {
	t.Parallel()
	m := &plannerMock{
		replies: []string{
			"the user wants a summary",
			"still talking instead of returning json",
			"and yet again",
		},
	}
	p := retrieval.NewPlanner(m, "test-model")
	p.MaxRetries = 2 // 1 initial + 2 retries = 3 attempts

	plan, usage, err := p.Plan(context.Background(), "Why does this fail to parse?")
	if err != nil {
		t.Fatalf("expected graceful nil error, got %v", err)
	}
	if plan != nil {
		t.Errorf("expected nil plan on parse failure, got %+v", plan)
	}
	if c := atomic.LoadInt32(&m.calls); c != 3 {
		t.Errorf("expected 3 LLM attempts (1 + 2 retries), got %d", c)
	}
	if usage.LLMCalls != 3 {
		t.Errorf("usage LLMCalls = %d, want 3 (all attempts counted)", usage.LLMCalls)
	}
}

// A successful retry after one bad JSON reply must return the parsed plan.
func TestPlannerSucceedsAfterRetry(t *testing.T) {
	t.Parallel()
	m := &plannerMock{
		replies: []string{
			"sorry, not json",
			jsonPlan(retrieval.Plan{Intent: "factual_lookup", IsMultiHop: false}),
		},
	}
	p := retrieval.NewPlanner(m, "test-model")
	plan, usage, err := p.Plan(context.Background(), "What is the recovery rate?")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan == nil || plan.Intent != "factual_lookup" {
		t.Errorf("plan after retry = %+v, want intent=factual_lookup", plan)
	}
	if usage.LLMCalls != 2 {
		t.Errorf("LLMCalls = %d, want 2 (1 failed + 1 succeeded)", usage.LLMCalls)
	}
}

// LLM transport failures should bubble up to the caller — these are
// distinct from parse failures (which we hide).
func TestPlannerTransportErrorBubbles(t *testing.T) {
	t.Parallel()
	m := &plannerMock{err: errors.New("provider 500")}
	p := retrieval.NewPlanner(m, "test-model")
	_, _, err := p.Plan(context.Background(), "any query")
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "provider 500") {
		t.Errorf("error %q should wrap the provider message", err)
	}
}

// Empty / whitespace queries are a no-op.
func TestPlannerEmptyQueryNoOp(t *testing.T) {
	t.Parallel()
	m := &plannerMock{}
	p := retrieval.NewPlanner(m, "test-model")

	plan, _, err := p.Plan(context.Background(), "   ")
	if err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		t.Errorf("empty query should return nil plan, got %+v", plan)
	}
	if c := atomic.LoadInt32(&m.calls); c != 0 {
		t.Errorf("empty query should issue no LLM calls, got %d", c)
	}
}

// A nil planner is safe to call — production wiring can pass nil when
// planning is disabled at the config level.
func TestNilPlannerSafe(t *testing.T) {
	t.Parallel()
	var p *retrieval.Planner
	plan, usage, err := p.Plan(context.Background(), "any query")
	if err != nil || plan != nil || usage.LLMCalls != 0 {
		t.Errorf("nil planner should be a no-op, got plan=%+v usage=%+v err=%v", plan, usage, err)
	}
}

func TestParsePlanCleansData(t *testing.T) {
	t.Parallel()
	raw := "```json\n" + jsonPlan(retrieval.Plan{
		Intent:           "  comparison  ",
		Entities:         []string{"  A  ", "", "B"},
		ExpectedDocAreas: []string{"results"},
		IsMultiHop:       true,
		SubQuestions:     []string{"What is A?", " ", "What is B?"},
	}) + "\n```"

	plan, err := retrieval.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Intent != "comparison" {
		t.Errorf("intent = %q, want comparison (trimmed)", plan.Intent)
	}
	if len(plan.Entities) != 2 {
		t.Errorf("entities = %v, want [A B] (empty stripped)", plan.Entities)
	}
	if len(plan.SubQuestions) != 2 {
		t.Errorf("sub_questions = %v, want 2 items (empty stripped)", plan.SubQuestions)
	}
	if !plan.IsMultiHop {
		t.Error("IsMultiHop should remain true (has sub-questions)")
	}
}

// Self-correction: IsMultiHop=true with no SubQuestions is meaningless;
// the parser clears the flag so the decomposer's fall-through fires.
func TestParsePlanSelfCorrectsMultiHopWithoutSubs(t *testing.T) {
	t.Parallel()
	raw := jsonPlan(retrieval.Plan{
		Intent:       "comparison",
		IsMultiHop:   true,
		SubQuestions: nil,
	})
	plan, err := retrieval.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.IsMultiHop {
		t.Error("IsMultiHop with no sub-questions should be coerced to false")
	}
}

// The cache returns defensive copies — mutating a returned plan must
// not corrupt the cached entry.
func TestPlannerCacheImmutability(t *testing.T) {
	t.Parallel()
	m := &plannerMock{
		replies: []string{
			jsonPlan(retrieval.Plan{
				Intent:       "list",
				Entities:     []string{"foo", "bar"},
				IsMultiHop:   true,
				SubQuestions: []string{"what is foo?", "what is bar?"},
			}),
		},
	}
	p := retrieval.NewPlanner(m, "test-model")

	first, _, err := p.Plan(context.Background(), "list the things")
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the returned plan.
	first.Intent = "MUTATED"
	first.Entities = nil
	first.SubQuestions[0] = "HIJACKED"

	// Second lookup should return a clean copy of the original.
	second, _, err := p.Plan(context.Background(), "list the things")
	if err != nil {
		t.Fatal(err)
	}
	if second.Intent != "list" {
		t.Errorf("cached intent corrupted: %q", second.Intent)
	}
	if len(second.Entities) != 2 || second.Entities[0] != "foo" {
		t.Errorf("cached entities corrupted: %v", second.Entities)
	}
	if second.SubQuestions[0] != "what is foo?" {
		t.Errorf("cached sub_questions corrupted: %v", second.SubQuestions)
	}
}
