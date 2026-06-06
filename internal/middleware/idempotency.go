package middleware

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── Idempotency-Key middleware ───────────────────────────────────────

// IdempotencyConfig controls the idempotency cache behaviour.
type IdempotencyConfig struct {
	// TTL is how long a cached response is retained. Default 24h.
	TTL time.Duration

	// PathPrefixes lists the URL path prefixes that the middleware
	// applies to. Only POST requests to these paths are cached.
	// Default: ["/v1/documents"].
	PathPrefixes []string
}

// cachedResponse stores the result of a previous request so it can be
// replayed when the same Idempotency-Key is seen again.
type cachedResponse struct {
	statusCode int
	body       []byte
	expiresAt  time.Time
}

// idempotencyCache is a thread-safe TTL map backed by sync.Map.
type idempotencyCache struct {
	entries sync.Map // key -> *cachedResponse
}

// get returns the cached response if it exists and has not expired.
func (c *idempotencyCache) get(key string) (*cachedResponse, bool) {
	v, ok := c.entries.Load(key)
	if !ok {
		return nil, false
	}
	cr := v.(*cachedResponse)
	if time.Now().After(cr.expiresAt) {
		c.entries.Delete(key)
		return nil, false
	}
	return cr, true
}

// set stores a response under the given key.
func (c *idempotencyCache) set(key string, cr *cachedResponse) {
	c.entries.Store(key, cr)
}

// reap removes expired entries. Called periodically from a background
// goroutine so the map does not grow without bound.
func (c *idempotencyCache) reap() {
	now := time.Now()
	c.entries.Range(func(key, value any) bool {
		if cr := value.(*cachedResponse); now.After(cr.expiresAt) {
			c.entries.Delete(key)
		}
		return true
	})
}

// ── Response recorder ────────────────────────────────────────────────

// responseRecorder captures the status code and body written by a
// downstream handler so they can be stored in the idempotency cache.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.statusCode = code
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	rr.body.Write(b) // capture for cache
	return rr.ResponseWriter.Write(b)
}

// ── Middleware constructor ────────────────────────────────────────────

// Idempotency returns middleware that caches POST responses keyed by the
// Idempotency-Key header. If a request carries a key that was seen
// before (within the TTL), the cached response is replayed without
// invoking the handler again.
//
// Only POST requests whose URL path matches one of the configured
// prefixes are subject to idempotency checking. All other requests pass
// through unchanged.
func Idempotency(cfg IdempotencyConfig) func(http.Handler) http.Handler {
	if cfg.TTL == 0 {
		cfg.TTL = 24 * time.Hour
	}
	if len(cfg.PathPrefixes) == 0 {
		cfg.PathPrefixes = []string{"/v1/documents"}
	}

	cache := &idempotencyCache{}

	// Background reaper: sweep expired entries every 10 minutes.
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cache.reap()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only apply to POST requests on matching paths.
			if r.Method != http.MethodPost || !matchesAnyPrefix(r.URL.Path, cfg.PathPrefixes) {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				// No key provided — proceed normally.
				next.ServeHTTP(w, r)
				return
			}

			// Check cache.
			if cr, ok := cache.get(key); ok {
				w.Header().Set("X-Idempotency-Replayed", "true")
				w.WriteHeader(cr.statusCode)
				_, _ = w.Write(cr.body) //nolint:errcheck // best-effort write to response
				return
			}

			// Record the response.
			rec := &responseRecorder{
				ResponseWriter: w,
				statusCode:     http.StatusOK, // default if WriteHeader is never called
			}
			next.ServeHTTP(rec, r)

			// Cache the result.
			cache.set(key, &cachedResponse{
				statusCode: rec.statusCode,
				body:       append([]byte(nil), rec.body.Bytes()...), // defensive copy
				expiresAt:  time.Now().Add(cfg.TTL),
			})
		})
	}
}

// matchesAnyPrefix reports whether path starts with any of the given
// prefixes.
func matchesAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
