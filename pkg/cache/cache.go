// Package cache provides an in-memory, TTL-aware, size-bounded LRU cache
// used by the retrieval layer to skip repeated LLM calls for the same
// (document, query, strategy, model) tuple.
//
// Design decisions:
//
//   - Zero external dependencies. The engine already has Postgres; wiring a
//     Redis dependency just for retrieval caching is over-engineering for
//     most self-hosters. An in-memory LRU is fast, simple, and restarts
//     cleanly (cold cache is safe; stale cache is worse).
//
//   - TTL is per-entry. Callers provide it at Set time so the retrieval
//     layer can adapt TTL to document freshness (e.g. shorter TTL right
//     after re-ingest).
//
//   - Thread-safe. Multiple goroutines (parallel queries, streaming, etc.)
//     share a single cache instance.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Cache is the interface the retrieval layer calls. Implementations MUST be
// safe for concurrent use.
type Cache interface {
	// Get retrieves a cached value by key. Returns nil and false if the key
	// is missing or expired.
	Get(key string) (any, bool)

	// Set stores a value under key with the given TTL. If the cache is full,
	// the least-recently-used entry is evicted first.
	Set(key string, value any, ttl time.Duration)

	// Delete removes a single key.
	Delete(key string)

	// Len returns the number of live (non-expired) entries. Primarily for
	// testing and metrics.
	Len() int

	// Clear removes all entries.
	Clear()

	// Stats returns cache hit/miss statistics.
	Stats() Stats
}

// Stats exposes runtime cache metrics.
type Stats struct {
	Hits       int64
	Misses     int64
	Evictions  int64
	Entries    int
	MaxEntries int
}

// HitRate returns the cache hit rate as a percentage (0.0–100.0). Returns
// 0 if no lookups have been made.
func (s Stats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total) * 100
}

// --- LRU implementation ---

// entry is a single cached value + expiry.
type entry struct {
	key       string
	value     any
	expiresAt time.Time
}

// LRU is a fixed-capacity, TTL-aware, least-recently-used cache.
//
// When capacity is reached, the least-recently-used entry is evicted to make
// room. Expired entries are evicted lazily (on access) and opportunistically
// during Set.
type LRU struct {
	mu         sync.Mutex
	maxEntries int
	items      map[string]*list.Element
	order      *list.List // front = most recent, back = least recent

	hits      int64
	misses    int64
	evictions int64

	// now is injectable for tests. nil = time.Now.
	now func() time.Time
}

// NewLRU constructs an LRU cache holding at most maxEntries items.
// A maxEntries of 0 is treated as 1024.
func NewLRU(maxEntries int) *LRU {
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	return &LRU{
		maxEntries: maxEntries,
		items:      make(map[string]*list.Element, maxEntries),
		order:      list.New(),
	}
}

// Get looks up a cached value. If found and not expired, it is promoted to
// the front (most-recently-used). Expired entries are silently evicted.
func (c *LRU) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}

	e := el.Value.(*entry)
	if c.nowFn().After(e.expiresAt) {
		// Expired — evict lazily.
		c.removeElement(el)
		c.misses++
		return nil, false
	}

	// Promote to front.
	c.order.MoveToFront(el)
	c.hits++
	return e.value, true
}

// Set stores key → value with the given TTL. If the cache is at capacity,
// the least-recently-used entry is evicted before insertion.
func (c *LRU) Set(key string, value any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.nowFn()

	// Update in-place if key already exists.
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		e := el.Value.(*entry)
		e.value = value
		e.expiresAt = now.Add(ttl)
		return
	}

	// Evict if at capacity.
	for c.order.Len() >= c.maxEntries {
		c.evictOldest()
	}

	e := &entry{
		key:       key,
		value:     value,
		expiresAt: now.Add(ttl),
	}
	el := c.order.PushFront(e)
	c.items[key] = el
}

// Delete removes a single key.
func (c *LRU) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
}

// Len returns the number of entries currently in the cache (may include
// entries that are expired but not yet lazily evicted).
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Clear removes all entries.
func (c *LRU) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element, c.maxEntries)
	c.order.Init()
}

// Stats returns a snapshot of cache metrics.
func (c *LRU) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits:       c.hits,
		Misses:     c.misses,
		Evictions:  c.evictions,
		Entries:    c.order.Len(),
		MaxEntries: c.maxEntries,
	}
}

func (c *LRU) evictOldest() {
	el := c.order.Back()
	if el == nil {
		return
	}
	c.removeElement(el)
}

func (c *LRU) removeElement(el *list.Element) {
	c.order.Remove(el)
	e := el.Value.(*entry)
	delete(c.items, e.key)
	c.evictions++
}

func (c *LRU) nowFn() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// --- Key helpers ---

// Key builds a cache key from the components that uniquely identify a
// retrieval query. The returned string is a hex-encoded SHA-256 digest.
func Key(documentID, query, strategy, model string) string {
	h := sha256.New()
	h.Write([]byte(documentID))
	h.Write([]byte{0})
	h.Write([]byte(query))
	h.Write([]byte{0})
	h.Write([]byte(strategy))
	h.Write([]byte{0})
	h.Write([]byte(model))
	return hex.EncodeToString(h.Sum(nil))
}
