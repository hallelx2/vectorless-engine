package handler

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestRouterParity is a divergence guard. The deployed cmd/server
// binary and the standalone cmd/engine binary serve overlapping route
// sets from two different routers (internal/handler vs internal/api).
// They have silently diverged before — the PageIndex redesign landed
// only on cmd/engine, leaving /v1/answer and /v1/answer/pageindex
// unreachable in production.
//
// This test walks the mounted chi router and asserts that the routes
// the deployed binary is contractually required to expose are present.
// If a future refactor drops one of the answer endpoints (or the
// per-request strategy override's /v1/query mount), this fails loudly
// instead of shipping a binary that's missing half the API.
func TestRouterParity(t *testing.T) {
	t.Parallel()

	// A zero Deps is enough to construct the router: handlers store
	// their dependencies and only dereference them at request time, so
	// Walk sees the full route table without any live backend.
	h := Router(Deps{})

	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("Router did not return a chi.Routes (got %T); cannot walk routes", h)
	}

	got := map[string]bool{}
	walk := func(method, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = true
		return nil
	}
	if err := chi.Walk(routes, walk); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}

	// The route set the deployed binary MUST expose. Anything added
	// here becomes a hard contract; drop a route and this test fails.
	want := []string{
		"POST /v1/query/",
		"POST /v1/answer",
		"POST /v1/answer/pageindex",
	}
	for _, route := range want {
		if !got[route] {
			t.Errorf("router is missing required route %q\nmounted routes: %v", route, sortedKeys(got))
		}
	}
}

// TestRouterMountsAnswerEndpoints is the focused assertion the task
// calls out explicitly: /v1/answer and /v1/answer/pageindex must be
// mounted on the deployed router. Kept separate from the broad parity
// set so a failure points straight at the answer-endpoint regression.
func TestRouterMountsAnswerEndpoints(t *testing.T) {
	t.Parallel()

	routes, ok := Router(Deps{}).(chi.Routes)
	if !ok {
		t.Fatal("Router did not return a chi.Routes")
	}

	found := map[string]bool{}
	_ = chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		found[route] = true
		return nil
	})

	for _, route := range []string{"/v1/answer", "/v1/answer/pageindex"} {
		if !found[route] {
			t.Errorf("deployed router must mount %q but does not", route)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Simple insertion sort to avoid pulling in sort just for a test
	// diagnostic; the route set is tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
