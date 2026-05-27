package retrieval

import (
	"sync"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/cache"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// ReplayStore is the persistence interface for replay-trace entries.
// Implementations MUST be safe for concurrent use; the HTTP layer
// writes from /v1/query and /v1/answer handlers and reads from
// /v1/replay handlers at the same time.
type ReplayStore interface {
	// Get retrieves a stored replay entry by trace token. Returns
	// (zero, false) when the token is unknown or the entry has been
	// evicted (LRU pressure or TTL).
	Get(token string) (ReplayEntry, bool)

	// Put stores a replay entry under the given trace token. If the
	// token is already present, the entry replaces it.
	Put(token string, entry ReplayEntry)
}

// ReplayEntry is what /v1/replay needs to reproduce a prior response
// byte-for-byte. ResponseJSON is the literal bytes of the original
// HTTP response — storing the marshalled response (not the underlying
// Go struct) is what guarantees byte-exact replay. Go's
// encoding/json sorts map keys lexicographically, so re-encoding the
// same map would already be deterministic, but persisting raw bytes
// removes any future doubt: it doesn't matter how the response is
// constructed, only what was actually returned.
//
// The remaining fields exist so the replay handler can validate the
// caller's claim — body.query and body.document_id must match
// entry.Query and entry.DocumentID, otherwise the engine returns 409
// with a specific reason.
type ReplayEntry struct {
	// DocumentID is the document the original request targeted.
	DocumentID tree.DocumentID

	// Query is the user query the original request carried.
	Query string

	// Model is the resolved LLM model name (after defaults). Stored
	// so future versions can validate model claims; today the trace
	// token already encodes the model, so a mismatch surfaces as
	// "trace_token not found" rather than a 409.
	Model string

	// SelectedIDs is the set of section IDs the strategy picked.
	// Stored for debugging and observability; not used by the replay
	// handler today (the response bytes already contain the IDs).
	SelectedIDs []tree.SectionID

	// ResponseJSON is the literal response body sent over the wire.
	// The replay handler writes this back verbatim with
	// Content-Type: application/json.
	ResponseJSON []byte

	// CreatedAt is when this entry was stored. Surfaced in logs.
	CreatedAt time.Time
}

// LRUReplayStore is an in-memory, TTL-aware, size-bounded replay
// store. It is a thin facade over pkg/cache.LRU: that cache is
// general-purpose (stores any) and concurrency-safe, so wrapping it
// avoids a duplicate LRU implementation. The facade enforces the
// ReplayEntry value type so callers cannot accidentally store the
// wrong shape.
//
// Capacity and TTL are configured at construction. Default capacity
// is 1024 entries, default TTL is 24h — sufficient for any realistic
// replay flow (audit, regression, debugging) while preventing
// unbounded memory growth.
//
// LRUReplayStore is NOT durable across process restarts. This is an
// intentional v1 limitation — the Phase 3.2 plan calls out persistent
// storage (Postgres-backed replay log + document versioning) as the
// next step.
type LRUReplayStore struct {
	// mu guards ttl; the underlying cache is already lock-free for
	// callers because pkg/cache.LRU has its own internal mutex.
	mu  sync.RWMutex
	ttl time.Duration

	c *cache.LRU
}

// Compile-time interface check.
var _ ReplayStore = (*LRUReplayStore)(nil)

// LRUReplayConfig is the configuration for an LRUReplayStore.
type LRUReplayConfig struct {
	// MaxEntries bounds the number of stored replay entries. Zero
	// defaults to 1024.
	MaxEntries int

	// TTL is how long an entry remains valid. Zero defaults to 24
	// hours.
	TTL time.Duration
}

// NewLRUReplayStore constructs an in-memory replay store with the
// given capacity and TTL. Zero values fall back to defaults.
func NewLRUReplayStore(cfg LRUReplayConfig) *LRUReplayStore {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &LRUReplayStore{
		ttl: ttl,
		c:   cache.NewLRU(cfg.MaxEntries),
	}
}

// Get implements ReplayStore.
func (s *LRUReplayStore) Get(token string) (ReplayEntry, bool) {
	if token == "" {
		return ReplayEntry{}, false
	}
	v, ok := s.c.Get(token)
	if !ok {
		return ReplayEntry{}, false
	}
	entry, ok := v.(ReplayEntry)
	if !ok {
		// Defensive: should never happen because Put only stores
		// ReplayEntry, but a corrupt entry shouldn't panic the
		// handler. Treat as a miss.
		return ReplayEntry{}, false
	}
	return entry, true
}

// Put implements ReplayStore.
func (s *LRUReplayStore) Put(token string, entry ReplayEntry) {
	if token == "" {
		return
	}
	s.mu.RLock()
	ttl := s.ttl
	s.mu.RUnlock()
	s.c.Set(token, entry, ttl)
}

// Len reports the current number of entries (including expired-but-
// not-yet-evicted entries). Primarily for tests and metrics.
func (s *LRUReplayStore) Len() int {
	return s.c.Len()
}
