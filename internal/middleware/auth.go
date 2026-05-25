// Package middleware provides HTTP middleware for the vectorless server.
//
// Each middleware is a func(http.Handler) http.Handler. The stack order
// is defined in the router package; these are building blocks.
package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// ── Principal ─────────────────────────────────────────────────────

// Principal represents the authenticated identity of a request.
// In the server, this is deliberately minimal — multi-tenant identity
// is the control plane's job.
type Principal struct {
	// ID is a stable identifier for the authenticated entity.
	// "anonymous" when auth is disabled.
	ID string

	// Authenticated is true when the request passed a real auth check
	// (not NoAuth).
	Authenticated bool
}

type principalKey struct{}

// PrincipalFromContext retrieves the Principal set by the auth
// middleware. Returns a zero-value Principal if none is set.
func PrincipalFromContext(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{}
}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// ── Authenticator interface ───────────────────────────────────────

// Authenticator validates an incoming request and returns the identity
// that made it. Returning a non-nil error rejects the request.
type Authenticator interface {
	// Authenticate inspects the request (typically the Authorization
	// header) and returns a Principal. A non-nil error means "reject".
	Authenticate(r *http.Request) (Principal, error)
}

// ── NoAuth ────────────────────────────────────────────────────────

// NoAuth always succeeds with an anonymous principal. This is the
// default when auth.mode is "none".
type NoAuth struct{}

// Authenticate returns an anonymous principal unconditionally.
func (NoAuth) Authenticate(_ *http.Request) (Principal, error) {
	return Principal{ID: "anonymous", Authenticated: false}, nil
}

// ── StaticAPIKey ──────────────────────────────────────────────────

// StaticAPIKey validates the Authorization: Bearer header against a
// single pre-configured key. Comparison uses crypto/subtle to prevent
// timing attacks. Suitable for self-hosters who need a simple gate.
type StaticAPIKey struct {
	key []byte
}

// NewStaticAPIKey creates a StaticAPIKey authenticator.
func NewStaticAPIKey(key string) *StaticAPIKey {
	return &StaticAPIKey{key: []byte(key)}
}

// Authenticate checks the Bearer token.
func (s *StaticAPIKey) Authenticate(r *http.Request) (Principal, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return Principal{}, errUnauthorized("missing Authorization header")
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == header {
		// No "Bearer " prefix found.
		return Principal{}, errUnauthorized("Authorization header must use Bearer scheme")
	}
	if subtle.ConstantTimeCompare([]byte(token), s.key) != 1 {
		return Principal{}, errUnauthorized("invalid API key")
	}
	return Principal{ID: "api_key", Authenticated: true}, nil
}

// ── Auth middleware ────────────────────────────────────────────────

// excludedPaths are routes that skip API-key authentication.
var excludedPaths = map[string]bool{
	"/v1/health":  true,
	"/v1/version": true,
	"/metrics":    true,
}

// excludedPrefixes lets a whole subtree skip API-key authentication.
// /internal/jobs/* is the QStash webhook endpoint — it carries its
// own Upstash signature header which the handler verifies, so the
// API key is meaningless (and unobtainable from QStash).
var excludedPrefixes = []string{
	"/internal/",
}

// Auth returns middleware that authenticates every request (except
// excluded paths) using the provided Authenticator. On success the
// Principal is stored in the request context.
func Auth(auth Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for health, version, metrics, and any path
			// in the excluded-prefix list (e.g. /internal/jobs/*).
			if excludedPaths[r.URL.Path] || hasExcludedPrefix(r.URL.Path) {
				p := Principal{ID: "anonymous", Authenticated: false}
				r = r.WithContext(withPrincipal(r.Context(), p))
				next.ServeHTTP(w, r)
				return
			}

			p, err := auth.Authenticate(r)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
				return
			}
			r = r.WithContext(withPrincipal(r.Context(), p))
			next.ServeHTTP(w, r)
		})
	}
}

// ── helpers ───────────────────────────────────────────────────────

func hasExcludedPrefix(path string) bool {
	for _, p := range excludedPrefixes {
		if len(path) >= len(p) && path[:len(p)] == p {
			return true
		}
	}
	return false
}

type authError struct{ msg string }

func (e authError) Error() string { return e.msg }

func errUnauthorized(msg string) error { return authError{msg: msg} }
