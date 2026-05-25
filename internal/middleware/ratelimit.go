package middleware

import (
	"net/http"
	"sync"
	"time"
)

// RateLimit implements a simple in-memory token bucket rate limiter.
// This is the coarse global rate limit described in SERVER.md. For
// per-principal limiting in multi-replica deploys, a Redis-backed
// implementation will replace this in Phase 3.
//
// If requestsPerMinute is <= 0, the middleware is a no-op passthrough.
func RateLimit(requestsPerMinute int) func(http.Handler) http.Handler {
	if requestsPerMinute <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	bucket := &tokenBucket{
		capacity: requestsPerMinute,
		tokens:   requestsPerMinute,
		rate:     float64(requestsPerMinute) / 60.0, // tokens per second
		last:     time.Now(),
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !bucket.allow() {
				w.Header().Set("Retry-After", "1")
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

// tokenBucket is a simple token bucket for rate limiting.
type tokenBucket struct {
	mu       sync.Mutex
	capacity int
	tokens   int
	rate     float64   // tokens per second
	last     time.Time // last refill time
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now

	// Refill tokens based on elapsed time.
	b.tokens += int(elapsed * b.rate)
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}

	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}
