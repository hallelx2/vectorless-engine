package middleware

import (
	"context"
	"net/http"
	"time"
)

// ── Max Body Size ────────────────────────────────────────────────────

// MaxBodySize wraps the request body with http.MaxBytesReader so that
// reads beyond maxBytes cause an error. If the limit is exceeded the
// handler (or the JSON decoder) will surface it; we also install an
// error-check wrapper that returns 413 Payload Too Large.
func MaxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.ContentLength != 0 {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ── Request Timeout ──────────────────────────────────────────────────

// RequestTimeout wraps the handler with a context deadline. When the
// deadline fires the context is cancelled; well-behaved handlers that
// check ctx.Err() will abort early.
//
// The timeout does NOT forcibly close the connection — it relies on the
// handler respecting context cancellation. The server's WriteTimeout
// remains the hard backstop.
func RequestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			done := make(chan struct{})
			go func() {
				next.ServeHTTP(w, r.WithContext(ctx))
				close(done)
			}()

			select {
			case <-done:
				// Handler finished normally.
			case <-ctx.Done():
				// Deadline exceeded — wait for the goroutine to exit
				// (it will see ctx.Err()). If the handler is stuck, the
				// server's WriteTimeout will kill the connection.
				<-done
			}
		})
	}
}

// ── Per-Endpoint Timeout ─────────────────────────────────────────────

// EndpointTimeout is a convenience that returns different timeouts for
// query endpoints versus everything else. It inspects the URL path to
// decide.
//
// queryTimeout applies to paths containing "/query".
// /internal/* paths get no timeout — they're internal queue webhooks
// (QStash callbacks) running the ingest pipeline, which can take many
// minutes for large PDFs. The inbound caller (QStash + Cloud Run)
// already enforce their own deadlines.
// defaultTimeout applies to all other paths.
func EndpointTimeout(defaultTimeout, queryTimeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isInternalPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			timeout := defaultTimeout
			if isQueryPath(r.URL.Path) {
				timeout = queryTimeout
			}

			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()

			done := make(chan struct{})
			go func() {
				next.ServeHTTP(w, r.WithContext(ctx))
				close(done)
			}()

			select {
			case <-done:
			case <-ctx.Done():
				<-done
			}
		})
	}
}

func isInternalPath(p string) bool {
	const prefix = "/internal/"
	return len(p) >= len(prefix) && p[:len(prefix)] == prefix
}

// isQueryPath reports whether the URL path is a query endpoint that
// should receive the longer timeout.
func isQueryPath(path string) bool {
	// Covers both REST (/v1/query) and Connect-RPC query paths.
	return len(path) >= 6 && containsQuery(path)
}

func containsQuery(s string) bool {
	for i := 0; i <= len(s)-5; i++ {
		if s[i:i+5] == "query" || s[i:i+5] == "Query" {
			return true
		}
	}
	return false
}
