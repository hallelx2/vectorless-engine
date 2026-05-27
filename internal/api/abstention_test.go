package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/config"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TestShouldAbstainAllBelow: every confidence under threshold → abstain.
func TestShouldAbstainAllBelow(t *testing.T) {
	t.Parallel()
	confidences := map[tree.SectionID]float64{"sec_a": 0.1, "sec_b": 0.2, "sec_c": 0.39}
	if !shouldAbstain(confidences, 0.4) {
		t.Error("all confidences below 0.4 must trigger abstention")
	}
}

// TestShouldAbstainOneAbove: any confidence at-or-above threshold → no abstain.
// The "all picks below" semantics is the spec's choice: if even one
// section has signal, surface it as evidence.
func TestShouldAbstainOneAbove(t *testing.T) {
	t.Parallel()
	confidences := map[tree.SectionID]float64{"sec_a": 0.1, "sec_b": 0.45}
	if shouldAbstain(confidences, 0.4) {
		t.Error("one pick at 0.45 should suppress abstention even when peers are low")
	}
}

// TestShouldAbstainBoundary: confidence == threshold counts as "above" so
// the engine is generous about evidence; the threshold is strict-below.
func TestShouldAbstainBoundary(t *testing.T) {
	t.Parallel()
	confidences := map[tree.SectionID]float64{"sec_a": 0.4}
	if shouldAbstain(confidences, 0.4) {
		t.Error("confidence == threshold must NOT trigger abstention (strict-below)")
	}
}

// TestShouldAbstainNilOrEmpty: missing confidence signal never abstains.
// This is the contract that keeps legacy-shape LLM responses working
// — the engine cannot abstain when it has no confidence to evaluate.
func TestShouldAbstainNilOrEmpty(t *testing.T) {
	t.Parallel()
	if shouldAbstain(nil, 0.4) {
		t.Error("nil confidences must NOT trigger abstention")
	}
	if shouldAbstain(map[tree.SectionID]float64{}, 0.4) {
		t.Error("empty confidences must NOT trigger abstention")
	}
}

// TestFilterConfidencesToIDsHappy verifies the helper restricts
// surfaced confidences to the IDs the response actually carries (post
// max_sections / re-rank truncation).
func TestFilterConfidencesToIDs(t *testing.T) {
	t.Parallel()
	src := map[tree.SectionID]float64{"a": 0.1, "b": 0.5, "c": 0.9}
	got := filterConfidencesToIDs(src, []tree.SectionID{"a", "c"})
	if len(got) != 2 {
		t.Fatalf("filtered length = %d, want 2", len(got))
	}
	if got["a"] != 0.1 || got["c"] != 0.9 {
		t.Errorf("filtered = %v", got)
	}
	if _, present := got["b"]; present {
		t.Error("b should have been filtered out")
	}
}

// TestFilterConfidencesNilStaysNil preserves the "no signal" sentinel
// across the helper.
func TestFilterConfidencesNilStaysNil(t *testing.T) {
	t.Parallel()
	if got := filterConfidencesToIDs(nil, []tree.SectionID{"a"}); got != nil {
		t.Errorf("nil input must produce nil output, got %v", got)
	}
	// All keys filtered out → nil too.
	if got := filterConfidencesToIDs(map[tree.SectionID]float64{"x": 0.5}, []tree.SectionID{"a"}); got != nil {
		t.Errorf("empty filtered result must produce nil, got %v", got)
	}
}

// TestStringKeyedConfidencesShape: the helper converts the typed map
// to JSON-friendly string keys for the wire response.
func TestStringKeyedConfidences(t *testing.T) {
	t.Parallel()
	got := stringKeyedConfidences(map[tree.SectionID]float64{"sec_a": 0.7})
	if got["sec_a"] != 0.7 {
		t.Errorf("converted map should preserve the value, got %v", got)
	}
	if stringKeyedConfidences(nil) != nil {
		t.Error("nil input must produce nil")
	}
}

// TestAbstentionEnabledOverride: per-request body field wins over server config.
func TestAbstentionEnabledOverride(t *testing.T) {
	t.Parallel()
	d := Deps{Abstain: config.AbstainBlock{Enabled: false}}
	if !d.abstentionEnabled(boolPtr(true)) {
		t.Error("body=true should override server=false")
	}
	d2 := Deps{Abstain: config.AbstainBlock{Enabled: true}}
	if d2.abstentionEnabled(boolPtr(false)) {
		t.Error("body=false should override server=true")
	}
}

// TestAbstentionEnabledFallsBackToConfig: when the body field is nil,
// the server config decides.
func TestAbstentionEnabledFallsBackToConfig(t *testing.T) {
	t.Parallel()
	d := Deps{Abstain: config.AbstainBlock{Enabled: true}}
	if !d.abstentionEnabled(nil) {
		t.Error("nil body should fall back to server=true")
	}
	d2 := Deps{Abstain: config.AbstainBlock{Enabled: false}}
	if d2.abstentionEnabled(nil) {
		t.Error("nil body should fall back to server=false")
	}
}

// --- Integration-style tests against handleQuery / handleAnswer ---
//
// These exercise the response-shape contracts: that all-low
// confidences yield an abstained response; that mixed
// (some-above-threshold) confidences yield a normal response; and
// that legacy responses (no confidences) never abstain.

// stubStrategy is a CostStrategy that returns canned IDs +
// confidences without touching any LLM.
type stubStrategy struct {
	ids         []tree.SectionID
	confidences map[tree.SectionID]float64
	usage       retrieval.Usage
	calls       int32
}

func (s *stubStrategy) Name() string { return "stub" }

func (s *stubStrategy) Select(ctx context.Context, t *tree.Tree, query string, budget retrieval.ContextBudget) ([]tree.SectionID, error) {
	atomic.AddInt32(&s.calls, 1)
	return s.ids, nil
}

func (s *stubStrategy) SelectWithCost(ctx context.Context, t *tree.Tree, query string, budget retrieval.ContextBudget) (*retrieval.Result, error) {
	atomic.AddInt32(&s.calls, 1)
	return &retrieval.Result{
		SelectedIDs: s.ids,
		Confidences: s.confidences,
		Usage:       s.usage,
		HopsTaken:   1,
	}, nil
}

// abstentionRouter wires only handleQuery / handleAnswer. We mock the
// strategy and bypass DB by passing a tiny in-memory tree-loader
// stub. The simplest way is to give the handler a Strategy that
// short-circuits before any storage read — done by also stubbing
// the storage to return empty content.
func abstentionRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Post("/query", d.handleQuery)
		r.Post("/answer", d.handleAnswer)
	})
	return r
}

// TestHandleQueryAbstainsOnAllLow: every confidence below threshold →
// the response is the abstention shape with sections=[] and
// abstained=true.
//
// We cannot run handleQuery without a DB-backed tree loader; instead,
// this test calls the helper functions on a Deps struct as the
// handler would, asserting the shape.
func TestRespondAbstained(t *testing.T) {
	t.Parallel()
	d := Deps{
		Strategy: &stubStrategy{ids: []tree.SectionID{"sec_a"}},
		Abstain:  config.AbstainBlock{Enabled: true, Below: 0.4},
	}
	confidences := map[tree.SectionID]float64{"sec_a": 0.12, "sec_b": 0.30}

	rec := httptest.NewRecorder()
	d.respondAbstained(rec, tree.DocumentID("doc_x"), "what is x?", confidences, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if v, _ := body["abstained"].(bool); !v {
		t.Error("response must carry abstained=true")
	}
	if v, _ := body["abstention_reason"].(string); !strings.Contains(v, "confidence") {
		t.Errorf("abstention_reason missing 'confidence': %q", v)
	}
	if v, _ := body["min_confidence_threshold"].(float64); v != 0.4 {
		t.Errorf("min_confidence_threshold = %v, want 0.4", v)
	}
	if v, _ := body["sections"].([]any); len(v) != 0 {
		t.Errorf("sections must be empty, got %v", v)
	}
	cc, ok := body["candidate_confidences"].(map[string]any)
	if !ok {
		t.Fatal("candidate_confidences missing")
	}
	if cc["sec_a"] != 0.12 {
		t.Errorf("sec_a confidence = %v, want 0.12", cc["sec_a"])
	}
}

// TestRespondAbstainedAnswer: same shape on /v1/answer. The synthesis
// call is skipped — answer is the canonical refusal string, citations
// is empty.
func TestRespondAbstainedAnswer(t *testing.T) {
	t.Parallel()
	d := Deps{
		Strategy: &stubStrategy{ids: []tree.SectionID{"sec_a"}},
		Abstain:  config.AbstainBlock{Enabled: true, Below: 0.4},
		Logger:   slog.Default(),
	}
	confidences := map[tree.SectionID]float64{"sec_a": 0.1}
	usage := retrieval.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, LLMCalls: 2}

	rec := httptest.NewRecorder()
	d.respondAbstainedAnswer(rec, tree.DocumentID("doc_x"), "q", confidences, nil, usage, time.Now())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if v, _ := body["abstained"].(bool); !v {
		t.Error("answer response must carry abstained=true")
	}
	if v, _ := body["answer"].(string); !strings.Contains(v, "cannot answer") {
		t.Errorf("answer must be the canonical refusal, got %q", v)
	}
	if v, _ := body["citations"].([]any); len(v) != 0 {
		t.Errorf("citations must be empty, got %v", v)
	}
	// Usage carried through (planning + retrieval — no synthesis).
	if u, ok := body["usage"].(map[string]any); !ok {
		t.Error("usage block missing")
	} else if u["llm_calls"].(float64) != 2 {
		t.Errorf("usage.llm_calls = %v, want 2", u["llm_calls"])
	}
}

// TestRespondAbstainedTraceTokenAbsent: replay isn't meaningful for
// an abstention (the engine produced no retrieval result); the
// response must NOT carry a trace_token so callers don't try to
// replay nothing.
func TestRespondAbstainedTraceTokenAbsent(t *testing.T) {
	t.Parallel()
	d := Deps{
		Strategy: &stubStrategy{},
		Abstain:  config.AbstainBlock{Enabled: true, Below: 0.4},
	}
	rec := httptest.NewRecorder()
	d.respondAbstained(rec, tree.DocumentID("doc_x"), "q", map[tree.SectionID]float64{"a": 0.1}, nil)

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, has := body["trace_token"]; has {
		t.Error("abstention response must NOT carry trace_token")
	}
}

// boolPtr is a tiny helper for the body-override tests.
func boolPtr(b bool) *bool { return &b }

// --- end-to-end through ServeHTTP without DB ---
//
// To exercise handleQuery / handleAnswer end-to-end we'd need a
// db.Pool. Instead we cover the in-handler logic by directly calling
// the helpers above (which is what the handler itself does on the
// abstention path) and by running the predicate tests through the
// handler-facing entrypoint via shouldAbstain + abstentionEnabled.
// A future test pass with a real test DB will exercise the full
// stack — for now, the abstention contract is unit-tested at the
// helper boundary, which is the only place the contract lives.

// mockLLMNeverCalled fails the test loudly if any LLM call lands.
// Used as a tripwire in the abstention path: synthesis must NOT
// run when /v1/answer abstains.
type mockLLMNeverCalled struct{ t *testing.T }

func (m mockLLMNeverCalled) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	m.t.Error("LLM should not be called on the abstention path")
	return &llmgate.Response{Content: ""}, nil
}

func (m mockLLMNeverCalled) CountTokens(ctx context.Context, s string) (int, error) {
	return len(s) / 4, nil
}

// TestRespondAbstainedAnswerSkipsSynthesis: the /v1/answer abstention
// helper must not invoke the LLM. We pass an LLM that explodes on
// any call so we'd see the test fail if synthesis leaks through.
func TestRespondAbstainedAnswerSkipsSynthesis(t *testing.T) {
	t.Parallel()
	d := Deps{
		Strategy: &stubStrategy{},
		Abstain:  config.AbstainBlock{Enabled: true, Below: 0.4},
		LLM:      mockLLMNeverCalled{t: t},
	}
	rec := httptest.NewRecorder()
	d.respondAbstainedAnswer(rec, tree.DocumentID("doc_x"), "q", map[tree.SectionID]float64{"a": 0.1}, nil, retrieval.Usage{}, time.Now())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// (Imports that won't otherwise be referenced by every test file go
// through small uses below so go vet is happy.)
var _ = bytes.NewReader
var _ = io.EOF
var _ = abstentionRouter
