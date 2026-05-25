package cache

import (
	"testing"
	"time"
)

func TestLRUBasicGetSet(t *testing.T) {
	t.Parallel()
	c := NewLRU(10)

	c.Set("a", "alpha", time.Minute)
	v, ok := c.Get("a")
	if !ok {
		t.Fatal("expected hit for key 'a'")
	}
	if v != "alpha" {
		t.Errorf("got %v, want alpha", v)
	}
}

func TestLRUMiss(t *testing.T) {
	t.Parallel()
	c := NewLRU(10)

	_, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("expected miss for nonexistent key")
	}
	s := c.Stats()
	if s.Misses != 1 {
		t.Errorf("misses = %d, want 1", s.Misses)
	}
}

func TestLRUExpiry(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := NewLRU(10)
	c.now = func() time.Time { return now }

	c.Set("short", "value", 100*time.Millisecond)

	// Still fresh.
	v, ok := c.Get("short")
	if !ok || v != "value" {
		t.Fatal("expected hit before expiry")
	}

	// Advance past expiry.
	c.now = func() time.Time { return now.Add(200 * time.Millisecond) }
	_, ok = c.Get("short")
	if ok {
		t.Fatal("expected miss after expiry")
	}
	// Entry should be lazily evicted.
	if c.Len() != 0 {
		t.Errorf("len = %d after expiry eviction, want 0", c.Len())
	}
}

func TestLRUEviction(t *testing.T) {
	t.Parallel()
	c := NewLRU(3)

	c.Set("a", 1, time.Hour)
	c.Set("b", 2, time.Hour)
	c.Set("c", 3, time.Hour)
	// Cache is full. Adding "d" should evict "a" (LRU).
	c.Set("d", 4, time.Hour)

	if _, ok := c.Get("a"); ok {
		t.Error("'a' should have been evicted")
	}
	if _, ok := c.Get("d"); !ok {
		t.Error("'d' should be present")
	}
	if c.Len() != 3 {
		t.Errorf("len = %d, want 3", c.Len())
	}
}

func TestLRUAccessPromotes(t *testing.T) {
	t.Parallel()
	c := NewLRU(3)

	c.Set("a", 1, time.Hour)
	c.Set("b", 2, time.Hour)
	c.Set("c", 3, time.Hour)

	// Touch "a" so it becomes most-recently-used.
	c.Get("a")

	// Now insert "d" — should evict "b" (now the LRU), not "a".
	c.Set("d", 4, time.Hour)

	if _, ok := c.Get("a"); !ok {
		t.Error("'a' was accessed and should not be evicted")
	}
	if _, ok := c.Get("b"); ok {
		t.Error("'b' should have been evicted (LRU)")
	}
}

func TestLRUUpdateInPlace(t *testing.T) {
	t.Parallel()
	c := NewLRU(10)

	c.Set("k", "v1", time.Hour)
	c.Set("k", "v2", time.Hour)

	v, ok := c.Get("k")
	if !ok || v != "v2" {
		t.Errorf("expected v2, got %v (ok=%v)", v, ok)
	}
	if c.Len() != 1 {
		t.Errorf("len = %d, want 1 (no duplicate entries)", c.Len())
	}
}

func TestLRUDelete(t *testing.T) {
	t.Parallel()
	c := NewLRU(10)

	c.Set("x", "y", time.Hour)
	c.Delete("x")
	if _, ok := c.Get("x"); ok {
		t.Error("expected miss after delete")
	}
	// Deleting nonexistent key is a no-op.
	c.Delete("nonexistent")
}

func TestLRUClear(t *testing.T) {
	t.Parallel()
	c := NewLRU(10)

	c.Set("a", 1, time.Hour)
	c.Set("b", 2, time.Hour)
	c.Clear()

	if c.Len() != 0 {
		t.Errorf("len = %d after clear, want 0", c.Len())
	}
}

func TestLRUStats(t *testing.T) {
	t.Parallel()
	c := NewLRU(2)

	c.Set("a", 1, time.Hour)
	c.Get("a") // hit
	c.Get("b") // miss

	s := c.Stats()
	if s.Hits != 1 {
		t.Errorf("hits = %d, want 1", s.Hits)
	}
	if s.Misses != 1 {
		t.Errorf("misses = %d, want 1", s.Misses)
	}
	if s.Entries != 1 {
		t.Errorf("entries = %d, want 1", s.Entries)
	}
	if s.MaxEntries != 2 {
		t.Errorf("max = %d, want 2", s.MaxEntries)
	}
}

func TestLRUHitRate(t *testing.T) {
	t.Parallel()
	s := Stats{Hits: 3, Misses: 1}
	if r := s.HitRate(); r != 75.0 {
		t.Errorf("hit rate = %f, want 75.0", r)
	}
	zero := Stats{}
	if r := zero.HitRate(); r != 0 {
		t.Errorf("zero hit rate = %f, want 0", r)
	}
}

func TestLRUDefaultCapacity(t *testing.T) {
	t.Parallel()
	c := NewLRU(0)
	if c.maxEntries != 1024 {
		t.Errorf("default capacity = %d, want 1024", c.maxEntries)
	}
}

func TestKeyDeterministic(t *testing.T) {
	t.Parallel()
	k1 := Key("doc1", "what is X?", "chunked-tree", "claude-3")
	k2 := Key("doc1", "what is X?", "chunked-tree", "claude-3")
	if k1 != k2 {
		t.Errorf("keys differ: %q vs %q", k1, k2)
	}
}

func TestKeyDiffersOnInput(t *testing.T) {
	t.Parallel()
	k1 := Key("doc1", "query", "strat", "model")
	k2 := Key("doc2", "query", "strat", "model")
	if k1 == k2 {
		t.Error("different doc IDs should produce different keys")
	}
}
