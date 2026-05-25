package middleware

import (
	"net/http"
	"sync"
	"time"
)

// PrincipalRateLimit implements per-principal rate limiting using an
// in-memory token bucket per unique principal ID. For multi-replica
// deploys, replace with a Redis-backed implementation (see Phase 3
// roadmap).
//
// When enabled, each authenticated principal gets their own token
// bucket. Anonymous principals (NoAuth) share a single bucket keyed
// by "anonymous".
//
// Config: rate_limit.per_principal_rpm (requests per minute per principal).
func PrincipalRateLimit(rpm int) func(http.Handler) http.Handler {
	if rpm <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	store := &bucketStore{
		buckets: make(map[string]*tokenBucket),
		rpm:     rpm,
	}

	// Periodically evict stale buckets (principals who haven't sent
	// a request in 10 minutes) to prevent unbounded memory growth.
	go store.reapLoop(10 * time.Minute)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFromContext(r.Context())
			key := p.ID
			if key == "" {
				key = "anonymous"
			}

			bucket := store.get(key)
			if !bucket.allow() {
				w.Header().Set("Retry-After", "1")
				w.Header().Set("X-RateLimit-Limit", intToStr(rpm))
				w.Header().Set("X-RateLimit-Remaining", "0")
				http.Error(w,
					`{"error":"rate limit exceeded"}`,
					http.StatusTooManyRequests,
				)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bucketStore manages per-principal token buckets.
type bucketStore struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rpm     int
}

func (s *bucketStore) get(key string) *tokenBucket {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.buckets[key]
	if !ok {
		b = &tokenBucket{
			capacity: s.rpm,
			tokens:   s.rpm,
			rate:     float64(s.rpm) / 60.0,
			last:     time.Now(),
		}
		s.buckets[key] = b
	}
	return b
}

func (s *bucketStore) reapLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		cutoff := time.Now().Add(-interval)
		for k, b := range s.buckets {
			b.mu.Lock()
			if b.last.Before(cutoff) {
				delete(s.buckets, k)
			}
			b.mu.Unlock()
		}
		s.mu.Unlock()
	}
}

func intToStr(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	// Simple itoa for small numbers.
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
