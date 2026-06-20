package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// handler that returns a per-org body and counts invocations, so we can tell a
// cached replay from a real pass-through.
func countingOrgHandler(calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("doc-for-" + r.Header.Get("X-Vectorless-Org")))
	})
}

func post(mw http.Handler, org, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", nil)
	if org != "" {
		req.Header.Set("X-Vectorless-Org", org)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	return rec
}

func body(rec *httptest.ResponseRecorder) string {
	return rec.Body.String()
}

func TestIdempotencySameOrgReplays(t *testing.T) {
	var calls atomic.Int64
	mw := Idempotency(IdempotencyConfig{})(countingOrgHandler(&calls))

	first := post(mw, "orgA", "k1")
	second := post(mw, "orgA", "k1")

	if calls.Load() != 1 {
		t.Fatalf("handler invoked %d times, want 1 (second should replay)", calls.Load())
	}
	if got := second.Header().Get("X-Idempotency-Replayed"); got != "true" {
		t.Fatalf("second response missing replay header, got %q", got)
	}
	if body(first) != body(second) {
		t.Fatalf("replay body %q != original %q", body(second), body(first))
	}
}

func TestIdempotencyDifferentOrgsDoNotCollide(t *testing.T) {
	// The core cross-tenant guarantee: two orgs sending the SAME opaque
	// Idempotency-Key must each hit the handler and get THEIR OWN response —
	// org B must never be handed org A's cached document.
	var calls atomic.Int64
	mw := Idempotency(IdempotencyConfig{})(countingOrgHandler(&calls))

	a := post(mw, "orgA", "shared-key")
	b := post(mw, "orgB", "shared-key")

	if calls.Load() != 2 {
		t.Fatalf("handler invoked %d times, want 2 (no cross-org collision)", calls.Load())
	}
	if b.Header().Get("X-Idempotency-Replayed") == "true" {
		t.Fatal("org B got a replayed response — cross-tenant leak")
	}
	if body(a) == body(b) {
		t.Fatalf("org B received org A's body %q — cross-tenant leak", body(b))
	}
	if body(b) != "doc-for-orgB" {
		t.Fatalf("org B body = %q, want doc-for-orgB", body(b))
	}
}

func TestIdempotencyNoKeyAlwaysPassesThrough(t *testing.T) {
	var calls atomic.Int64
	mw := Idempotency(IdempotencyConfig{})(countingOrgHandler(&calls))

	post(mw, "orgA", "")
	post(mw, "orgA", "")

	if calls.Load() != 2 {
		t.Fatalf("handler invoked %d times, want 2 (no key = no caching)", calls.Load())
	}
}
