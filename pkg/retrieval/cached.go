package retrieval

import (
	"context"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/cache"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Cached wraps any Strategy and caches its Select results in an LRU.
//
// Cache key = sha256(document_id || query || strategy_name || model_name).
// The cached value is the []tree.SectionID slice; the wrapping is
// intentionally transparent — callers interact with the standard Strategy
// interface and don't know caching is present.
//
// Cached also implements CostStrategy if the wrapped strategy does. On a
// cache hit, CostUSD and LLMCalls are both zero (no LLM call was made).
type Cached struct {
	inner Strategy
	cache cache.Cache
	ttl   time.Duration
}

// Compile-time interface checks.
var (
	_ Strategy     = (*Cached)(nil)
	_ CostStrategy = (*Cached)(nil)
)

// CachedConfig is the configuration for the caching wrapper.
type CachedConfig struct {
	// MaxEntries is the maximum number of cached retrieval results.
	// Zero defaults to 1024.
	MaxEntries int

	// TTL is how long a cached result remains valid. Zero defaults to
	// 10 minutes. Set to a shorter value for frequently re-ingested
	// documents.
	TTL time.Duration
}

// NewCached wraps inner with an LRU retrieval cache.
func NewCached(inner Strategy, cfg CachedConfig) *Cached {
	if cfg.TTL == 0 {
		cfg.TTL = 10 * time.Minute
	}
	return &Cached{
		inner: inner,
		cache: cache.NewLRU(cfg.MaxEntries),
		ttl:   cfg.TTL,
	}
}

// Name delegates to the inner strategy.
func (c *Cached) Name() string { return c.inner.Name() }

// Select checks the cache first. On a miss it delegates to the inner
// strategy and caches the result.
func (c *Cached) Select(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) ([]tree.SectionID, error) {
	key := cache.Key(string(t.DocumentID), query, c.inner.Name(), budget.ModelName)

	if v, ok := c.cache.Get(key); ok {
		return v.([]tree.SectionID), nil
	}

	ids, err := c.inner.Select(ctx, t, query, budget)
	if err != nil {
		return nil, err
	}

	c.cache.Set(key, ids, c.ttl)
	return ids, nil
}

// SelectWithCost checks the cache first. On a hit it returns zero usage
// (no LLM call was made). On a miss it delegates to the inner strategy's
// SelectWithCost if available, otherwise falls back to Select.
func (c *Cached) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) (*Result, error) {
	key := cache.Key(string(t.DocumentID), query, c.inner.Name(), budget.ModelName)

	if v, ok := c.cache.Get(key); ok {
		return &Result{
			SelectedIDs: v.([]tree.SectionID),
			Reasoning:   "cached",
			ModelUsed:   budget.ModelName,
			Usage:       Usage{}, // zero — no LLM call
		}, nil
	}

	var result *Result
	if cs, ok := c.inner.(CostStrategy); ok {
		var err error
		result, err = cs.SelectWithCost(ctx, t, query, budget)
		if err != nil {
			return nil, err
		}
	} else {
		ids, err := c.inner.Select(ctx, t, query, budget)
		if err != nil {
			return nil, err
		}
		result = &Result{SelectedIDs: ids}
	}

	c.cache.Set(key, result.SelectedIDs, c.ttl)
	return result, nil
}

// SelectStream delegates to the inner strategy's SelectStream if it
// implements StreamStrategy. Streaming results are NOT cached because
// the stream is consumed as events arrive; the non-streaming Select path
// should be used when caching is desired.
func (c *Cached) SelectStream(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) <-chan StreamEvent {
	if ss, ok := c.inner.(StreamStrategy); ok {
		return ss.SelectStream(ctx, t, query, budget)
	}
	// If the inner strategy doesn't support streaming, return a channel
	// with an error event so the caller discovers it immediately.
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{
		Type:    EventError,
		Message: "inner strategy does not support streaming",
	}
	close(ch)
	return ch
}

// CacheStats exposes the underlying cache metrics.
func (c *Cached) CacheStats() cache.Stats {
	return c.cache.Stats()
}

// InvalidateDocument removes all cached results for a document. This is
// useful after re-ingest. Since cache keys are hashed, we can't delete by
// prefix; instead we expose this as a signal for the caller to Clear if
// needed. For single-document invalidation, the caller should track keys.
func (c *Cached) InvalidateAll() {
	c.cache.Clear()
}
