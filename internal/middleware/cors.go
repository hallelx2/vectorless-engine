package middleware

import (
	"net/http"
	"strconv"
	"strings"
)

// CORSConfig controls Cross-Origin Resource Sharing behaviour.
type CORSConfig struct {
	// AllowedOrigins is the list of origins permitted to access the API.
	// Use ["*"] during development to allow any origin.
	AllowedOrigins []string

	// MaxAge is the value of Access-Control-Max-Age in seconds.
	// Browsers cache the preflight response for this duration.
	MaxAge int
}

// defaultCORSHeaders are the headers every CORS response carries.
var (
	corsAllowHeaders  = "Content-Type, Authorization, X-Request-ID, Connect-Protocol-Version"
	corsAllowMethods  = "GET, POST, DELETE, OPTIONS"
	corsExposeHeaders = "X-Request-ID"
)

// CORS returns middleware that sets the appropriate Access-Control-*
// headers and short-circuits preflight OPTIONS requests. It must be
// placed before any other middleware so browsers receive CORS headers
// even when subsequent middleware rejects the request (e.g. auth).
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	// Pre-compute origin lookup.
	allowAll := len(cfg.AllowedOrigins) == 0
	origins := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAll = true
		}
		origins[o] = struct{}{}
	}

	maxAge := strconv.Itoa(cfg.MaxAge)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Determine which origin to echo back.
			var allowOrigin string
			switch {
			case origin == "":
				// Not a CORS request; skip origin header but still
				// let the request through.
			case allowAll:
				allowOrigin = "*"
			default:
				if _, ok := origins[origin]; ok {
					allowOrigin = origin
				}
			}

			if allowOrigin != "" {
				w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
				w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
				w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
				w.Header().Set("Access-Control-Max-Age", maxAge)

				// When reflecting a specific origin (not "*"), tell
				// caches that the response varies by Origin.
				if allowOrigin != "*" {
					w.Header().Set("Vary", "Origin")
				}
			}

			// Preflight: return 204 immediately — no need to hit the
			// router or any downstream middleware.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// originMatches is a helper that checks if an origin matches any of the
// patterns in the allowed list. It supports exact matches only; for
// wildcard sub-domain patterns extend this function.
//
//nolint:unused,staticcheck // CORS matcher (staged for wildcard-origin support)
func originMatches(origin string, patterns []string) bool {
	for _, p := range patterns {
		if strings.EqualFold(origin, p) {
			return true
		}
	}
	return false
}
