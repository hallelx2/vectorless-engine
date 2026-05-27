package retrieval_test

import (
	"regexp"
	"testing"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// hexToken matches a lower-case sha256 hex digest exactly 64 chars long.
var hexToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestComputeTraceTokenShape(t *testing.T) {
	t.Parallel()

	tok := retrieval.ComputeTraceToken(
		"doc_x", "1", "claude-sonnet-4-5",
		[]tree.SectionID{"sec_a", "sec_b"},
	)
	if !hexToken.MatchString(tok) {
		t.Fatalf("trace token must be 64-char lowercase hex, got %q", tok)
	}
}

// Determinism: same inputs → same token, always.
func TestComputeTraceTokenDeterministic(t *testing.T) {
	t.Parallel()

	ids := []tree.SectionID{"sec_a", "sec_b", "sec_c"}
	a := retrieval.ComputeTraceToken("doc_x", "1", "model", ids)
	b := retrieval.ComputeTraceToken("doc_x", "1", "model", ids)
	if a != b {
		t.Fatalf("same inputs must produce same token: %q vs %q", a, b)
	}
}

// Sort invariance: permuting the IDs must not change the token. This is
// the contract the replay endpoint relies on — two strategies that pick
// the same SET of sections produce byte-identical replay tokens
// regardless of reasoning order.
func TestComputeTraceTokenSortInvariant(t *testing.T) {
	t.Parallel()

	a := retrieval.ComputeTraceToken("doc_x", "1", "model",
		[]tree.SectionID{"sec_a", "sec_b", "sec_c"})
	b := retrieval.ComputeTraceToken("doc_x", "1", "model",
		[]tree.SectionID{"sec_c", "sec_a", "sec_b"})
	c := retrieval.ComputeTraceToken("doc_x", "1", "model",
		[]tree.SectionID{"sec_b", "sec_c", "sec_a"})
	if a != b || b != c {
		t.Fatalf("permuted IDs must produce same token: a=%q b=%q c=%q", a, b, c)
	}
}

// Sort invariance must not mutate the caller's slice. Computing the
// token over an already-sorted slice should leave a manually-sorted
// reference equal.
func TestComputeTraceTokenDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	ids := []tree.SectionID{"sec_c", "sec_a", "sec_b"}
	before := make([]tree.SectionID, len(ids))
	copy(before, ids)

	_ = retrieval.ComputeTraceToken("doc_x", "1", "model", ids)
	for i, id := range ids {
		if id != before[i] {
			t.Fatalf("input mutated at idx %d: before=%q after=%q", i, before[i], id)
		}
	}
}

// Input sensitivity: changing any single component must change the
// token. Without this, replay would accept queries against a different
// document or model and return a stale response.
func TestComputeTraceTokenInputSensitivity(t *testing.T) {
	t.Parallel()

	base := retrieval.ComputeTraceToken("doc_x", "1", "model",
		[]tree.SectionID{"sec_a", "sec_b"})

	cases := []struct {
		name string
		tok  string
	}{
		{"different doc", retrieval.ComputeTraceToken("doc_y", "1", "model",
			[]tree.SectionID{"sec_a", "sec_b"})},
		{"different version", retrieval.ComputeTraceToken("doc_x", "2", "model",
			[]tree.SectionID{"sec_a", "sec_b"})},
		{"different model", retrieval.ComputeTraceToken("doc_x", "1", "other",
			[]tree.SectionID{"sec_a", "sec_b"})},
		{"different ids", retrieval.ComputeTraceToken("doc_x", "1", "model",
			[]tree.SectionID{"sec_a", "sec_c"})},
		{"superset ids", retrieval.ComputeTraceToken("doc_x", "1", "model",
			[]tree.SectionID{"sec_a", "sec_b", "sec_c"})},
		{"empty ids", retrieval.ComputeTraceToken("doc_x", "1", "model", nil)},
	}
	for _, c := range cases {
		if c.tok == base {
			t.Errorf("%s: token did not change (still %q)", c.name, c.tok)
		}
	}
}

// Pathological IDs containing commas (or other delimiter-ish bytes)
// must not collide via the joining strategy. Two distinct ID sets that
// would collide under naive comma-join must produce distinct tokens.
func TestComputeTraceTokenAvoidsDelimiterCollision(t *testing.T) {
	t.Parallel()

	// Naive "a,b"+","+"c" join would equal "a"+","+"b,c" — using a NUL
	// separator inside the hash input avoids the collision because IDs
	// can't contain NULs in practice (and even if they could, the hash
	// would still distinguish them).
	a := retrieval.ComputeTraceToken("doc_x", "1", "model",
		[]tree.SectionID{"a,b", "c"})
	b := retrieval.ComputeTraceToken("doc_x", "1", "model",
		[]tree.SectionID{"a", "b,c"})
	if a == b {
		t.Fatalf("comma-delimited IDs must not collide: both produced %q", a)
	}
}

// Empty selection still produces a valid 64-char token — replay must
// work for "we read the document and nothing was relevant" outcomes,
// which are legitimate retrieval results.
func TestComputeTraceTokenEmptySelection(t *testing.T) {
	t.Parallel()

	tok := retrieval.ComputeTraceToken("doc_x", "1", "model", nil)
	if !hexToken.MatchString(tok) {
		t.Fatalf("empty selection should still produce valid hex token, got %q", tok)
	}
}

// SystemPromptVersion is folded into the token so an edit to the
// retrieval system prompt invalidates previously-cached replay
// entries. This test pins the constant so a change to its value is a
// deliberate decision (the assertion fails when SystemPromptVersion is
// bumped, prompting the author to acknowledge replay invalidation).
func TestSystemPromptVersionStable(t *testing.T) {
	t.Parallel()

	if retrieval.SystemPromptVersion != "v1" {
		t.Errorf("SystemPromptVersion changed to %q; bump replay docs and ensure existing replay entries are intentionally invalidated", retrieval.SystemPromptVersion)
	}
}
