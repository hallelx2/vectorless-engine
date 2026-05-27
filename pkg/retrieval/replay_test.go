package retrieval_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TestLRUReplayStoreBasicPutGet covers the happy path: put an entry,
// get it back with byte-identical ResponseJSON.
func TestLRUReplayStoreBasicPutGet(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
		MaxEntries: 10,
		TTL:        time.Minute,
	})

	want := retrieval.ReplayEntry{
		DocumentID:   "doc_x",
		Query:        "what is x?",
		Model:        "claude-sonnet-4-5",
		SelectedIDs:  []tree.SectionID{"sec_a", "sec_b"},
		ResponseJSON: []byte(`{"answer":"hello","strategy":"chunked-tree"}`),
		CreatedAt:    time.Now(),
	}
	store.Put("token-abc", want)

	got, ok := store.Get("token-abc")
	if !ok {
		t.Fatal("expected entry for token-abc")
	}
	if got.DocumentID != want.DocumentID || got.Query != want.Query {
		t.Errorf("entry mismatch: got %+v, want %+v", got, want)
	}
	if string(got.ResponseJSON) != string(want.ResponseJSON) {
		t.Errorf("ResponseJSON not preserved verbatim: got %q want %q",
			got.ResponseJSON, want.ResponseJSON)
	}
}

// TestLRUReplayStoreMiss covers the unknown-token path.
func TestLRUReplayStoreMiss(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	if _, ok := store.Get("nonexistent"); ok {
		t.Error("expected miss for unknown token")
	}
}

// TestLRUReplayStoreEmptyToken ensures the store never accepts the
// empty trace token as a key — that would let callers mass-evict the
// LRU by Put-ing an empty token in a tight loop, and Get("") must
// always miss.
func TestLRUReplayStoreEmptyToken(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{MaxEntries: 10})
	store.Put("", retrieval.ReplayEntry{
		ResponseJSON: []byte(`{"x":1}`),
	})
	if _, ok := store.Get(""); ok {
		t.Error("empty trace token must always miss on Get")
	}
	if store.Len() != 0 {
		t.Errorf("empty trace token must not be stored; Len=%d", store.Len())
	}
}

// TestLRUReplayStoreEviction asserts that the LRU honours its
// capacity bound: pushing beyond MaxEntries evicts the
// least-recently-used entry.
func TestLRUReplayStoreEviction(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
		MaxEntries: 3,
		TTL:        time.Hour,
	})

	for i, tok := range []string{"a", "b", "c"} {
		store.Put(tok, retrieval.ReplayEntry{
			ResponseJSON: []byte(fmt.Sprintf(`{"i":%d}`, i)),
		})
	}
	// Adding "d" must evict "a" (LRU).
	store.Put("d", retrieval.ReplayEntry{ResponseJSON: []byte(`{"i":3}`)})

	if _, ok := store.Get("a"); ok {
		t.Error("'a' should have been evicted as LRU")
	}
	if _, ok := store.Get("d"); !ok {
		t.Error("'d' should be present after Put")
	}
	if got := store.Len(); got != 3 {
		t.Errorf("Len = %d, want 3 (capacity)", got)
	}
}

// TestLRUReplayStoreTTLExpiry asserts that an entry past its TTL is
// no longer returned by Get. We can't sleep-wait reliably in tests,
// so we exercise the TTL path by using a very short TTL plus
// Sleep — fast enough to keep the test under a second.
func TestLRUReplayStoreTTLExpiry(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
		MaxEntries: 10,
		TTL:        50 * time.Millisecond,
	})

	store.Put("short", retrieval.ReplayEntry{
		ResponseJSON: []byte(`{"v":1}`),
	})

	// Immediately readable.
	if _, ok := store.Get("short"); !ok {
		t.Fatal("expected hit before expiry")
	}

	// Wait past TTL. 200ms gives generous slack vs the 50ms TTL.
	time.Sleep(200 * time.Millisecond)

	if _, ok := store.Get("short"); ok {
		t.Error("expected miss after TTL expiry")
	}
}

// TestLRUReplayStoreUpdateInPlace asserts that Put-ing the same
// token twice replaces (not duplicates) the entry. This matters
// because the API surface re-Puts on a replay (idempotent retry).
func TestLRUReplayStoreUpdateInPlace(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
		MaxEntries: 10,
		TTL:        time.Hour,
	})
	store.Put("k", retrieval.ReplayEntry{ResponseJSON: []byte(`{"v":1}`)})
	store.Put("k", retrieval.ReplayEntry{ResponseJSON: []byte(`{"v":2}`)})

	got, ok := store.Get("k")
	if !ok {
		t.Fatal("expected hit for replaced key")
	}
	if string(got.ResponseJSON) != `{"v":2}` {
		t.Errorf("expected updated value v:2, got %q", got.ResponseJSON)
	}
	if store.Len() != 1 {
		t.Errorf("Len = %d, want 1 (single key, no duplicate)", store.Len())
	}
}

// TestLRUReplayStoreThreadSafety hammers Put/Get from many goroutines
// to surface any race condition under -race. The assertion is that
// every Get either returns a complete ReplayEntry or misses cleanly;
// the store must not corrupt any individual entry or panic.
func TestLRUReplayStoreThreadSafety(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
		MaxEntries: 256,
		TTL:        time.Hour,
	})

	const workers = 16
	const opsPerWorker = 200

	var hits, misses int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				tok := fmt.Sprintf("w%d-i%d", w, i)
				resp := []byte(fmt.Sprintf(`{"w":%d,"i":%d}`, w, i))
				store.Put(tok, retrieval.ReplayEntry{
					DocumentID:   tree.DocumentID(fmt.Sprintf("doc_%d", w)),
					Query:        "q",
					ResponseJSON: resp,
				})
				got, ok := store.Get(tok)
				if !ok {
					atomic.AddInt64(&misses, 1)
					continue
				}
				atomic.AddInt64(&hits, 1)
				if string(got.ResponseJSON) != string(resp) {
					t.Errorf("ResponseJSON corruption: got %q want %q",
						got.ResponseJSON, resp)
					return
				}
			}
		}()
	}
	wg.Wait()

	// We don't require every Get to hit (LRU may evict between Put
	// and Get under contention) — the test surfaces races, not
	// hit-rate guarantees.
	if hits+misses != workers*opsPerWorker {
		t.Errorf("op count mismatch: hits+misses=%d expected %d",
			hits+misses, workers*opsPerWorker)
	}
}

// TestLRUReplayStoreDefaults asserts that NewLRUReplayStore applies
// the documented defaults when zero values are passed.
func TestLRUReplayStoreDefaults(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{})
	// Cannot inspect TTL directly without exposing an accessor, so
	// exercise it indirectly: a Put followed by an immediate Get
	// must hit — TTL must NOT default to zero (which would make
	// every entry immediately expired).
	store.Put("token", retrieval.ReplayEntry{
		ResponseJSON: []byte(`{"x":1}`),
	})
	if _, ok := store.Get("token"); !ok {
		t.Error("default TTL must be non-zero; Put then immediate Get missed")
	}
}

// TestLRUReplayStoreByteExactness asserts that the stored ResponseJSON
// is preserved byte-for-byte (no normalisation, no whitespace
// trimming) — replay's whole value prop is "same response down to
// the byte".
func TestLRUReplayStoreByteExactness(t *testing.T) {
	t.Parallel()
	store := retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
		MaxEntries: 10,
		TTL:        time.Hour,
	})

	// Include unusual whitespace and unicode so any normalisation
	// pass would visibly mangle it.
	tricky := []byte(`{"answer":"hello\n  world", "emoji":"❤", "k":  42}`)
	store.Put("t", retrieval.ReplayEntry{ResponseJSON: tricky})

	got, ok := store.Get("t")
	if !ok {
		t.Fatal("expected hit")
	}
	if len(got.ResponseJSON) != len(tricky) {
		t.Errorf("length drift: got %d want %d", len(got.ResponseJSON), len(tricky))
	}
	for i := range tricky {
		if got.ResponseJSON[i] != tricky[i] {
			t.Errorf("byte %d differs: got 0x%x want 0x%x",
				i, got.ResponseJSON[i], tricky[i])
			break
		}
	}
}
