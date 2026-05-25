package retrieval_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

func TestCachedHit(t *testing.T) {
	t.Parallel()

	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a"}}
	inner := retrieval.NewSinglePass(m)
	cached := retrieval.NewCached(inner, retrieval.CachedConfig{
		MaxEntries: 100,
		TTL:        time.Minute,
	})

	budget := retrieval.ContextBudget{MaxTokens: 100000}

	// First call → miss → LLM called.
	ids1, err := cached.Select(context.Background(), tr, "query A", budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids1) != 1 || ids1[0] != "sec_a" {
		t.Errorf("first call: want [sec_a], got %v", ids1)
	}
	if c := atomic.LoadInt32(&m.calls); c != 1 {
		t.Fatalf("want 1 LLM call, got %d", c)
	}

	// Second call with same inputs → hit → LLM NOT called.
	ids2, err := cached.Select(context.Background(), tr, "query A", budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids2) != 1 || ids2[0] != "sec_a" {
		t.Errorf("cached call: want [sec_a], got %v", ids2)
	}
	if c := atomic.LoadInt32(&m.calls); c != 1 {
		t.Fatalf("want still 1 LLM call (cached), got %d", c)
	}
}

func TestCachedDifferentQueryMisses(t *testing.T) {
	t.Parallel()

	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a", "sec_b"}}
	inner := retrieval.NewSinglePass(m)
	cached := retrieval.NewCached(inner, retrieval.CachedConfig{TTL: time.Minute})

	budget := retrieval.ContextBudget{MaxTokens: 100000}

	_, _ = cached.Select(context.Background(), tr, "query A", budget)
	_, _ = cached.Select(context.Background(), tr, "query B", budget)

	if c := atomic.LoadInt32(&m.calls); c != 2 {
		t.Errorf("different queries should each call LLM: want 2, got %d", c)
	}
}

func TestCachedSelectWithCostHit(t *testing.T) {
	t.Parallel()

	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_b"}}
	inner := retrieval.NewSinglePass(m)
	cached := retrieval.NewCached(inner, retrieval.CachedConfig{TTL: time.Minute})

	budget := retrieval.ContextBudget{MaxTokens: 100000, ModelName: "test-model"}

	// First call → miss.
	r1, err := cached.SelectWithCost(context.Background(), tr, "q", budget)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Usage.LLMCalls != 1 {
		t.Errorf("first call LLMCalls = %d, want 1", r1.Usage.LLMCalls)
	}

	// Second call → hit, zero cost.
	r2, err := cached.SelectWithCost(context.Background(), tr, "q", budget)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Usage.LLMCalls != 0 {
		t.Errorf("cached call LLMCalls = %d, want 0", r2.Usage.LLMCalls)
	}
	if r2.Reasoning != "cached" {
		t.Errorf("cached reasoning = %q, want 'cached'", r2.Reasoning)
	}
}

func TestCachedName(t *testing.T) {
	t.Parallel()

	m := &mockLLM{}
	inner := retrieval.NewSinglePass(m)
	cached := retrieval.NewCached(inner, retrieval.CachedConfig{})

	if cached.Name() != "single-pass" {
		t.Errorf("Name() = %q, want 'single-pass'", cached.Name())
	}
}

func TestCachedStats(t *testing.T) {
	t.Parallel()

	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a"}}
	inner := retrieval.NewSinglePass(m)
	cached := retrieval.NewCached(inner, retrieval.CachedConfig{
		MaxEntries: 50,
		TTL:        time.Minute,
	})

	budget := retrieval.ContextBudget{MaxTokens: 100000}

	_, _ = cached.Select(context.Background(), tr, "q", budget) // miss
	_, _ = cached.Select(context.Background(), tr, "q", budget) // hit

	stats := cached.CacheStats()
	if stats.Hits != 1 {
		t.Errorf("hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("misses = %d, want 1", stats.Misses)
	}
	if stats.MaxEntries != 50 {
		t.Errorf("max = %d, want 50", stats.MaxEntries)
	}
}

func TestCachedInvalidateAll(t *testing.T) {
	t.Parallel()

	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_c"}}
	inner := retrieval.NewSinglePass(m)
	cached := retrieval.NewCached(inner, retrieval.CachedConfig{TTL: time.Minute})

	budget := retrieval.ContextBudget{MaxTokens: 100000}

	_, _ = cached.Select(context.Background(), tr, "q", budget) // miss → call 1
	cached.InvalidateAll()
	_, _ = cached.Select(context.Background(), tr, "q", budget) // miss again → call 2

	if c := atomic.LoadInt32(&m.calls); c != 2 {
		t.Errorf("after invalidation, want 2 calls, got %d", c)
	}
}
