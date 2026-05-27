package ingest

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TestSummaryForReturnsAxes asserts the happy path: the LLM returns a
// valid JSON object with the four axes, and the parser surfaces them
// verbatim while normalising the one-line into the structure callers
// downstream rely on. This is the central contract the retrieval
// prompt + the DB persistence layer both lean on.
func TestSummaryForReturnsAxes(t *testing.T) {
	t.Parallel()

	const cannedJSON = `{
        "topics":   ["debt", "long-term-obligations"],
        "entities": ["3M Company", "JPMorgan", "Q3 2024"],
        "numbers":  ["$4.2B", "2.8%", "2034"],
        "one_line": "Long-term debt issued by 3M Company and serviced via JPMorgan."
    }`
	m := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			if !req.JSONMode {
				t.Errorf("structured summary call should request JSON mode")
			}
			return &llmgate.Response{Content: cannedJSON}, nil
		},
	}
	p := &Pipeline{
		LLM:                    m,
		Logger:                 slog.Default(),
		SummaryMaxChars:        4000,
		SummaryModel:           "m",
		SummaryAxesEnabled:     true,
		SummaryAxesMaxTopics:   4,
		SummaryAxesMaxEntities: 8,
		SummaryAxesMaxNumbers:  6,
	}
	sec := db.Section{ID: tree.SectionID("sec_a"), Title: "Long-Term Debt"}

	// Use a non-nil childLines so summaryFor takes the internal-node
	// path and skips the storage fetch — tests build a Pipeline literal
	// without Storage. The content seeded here is what the LLM "sees".
	childLines := []string{"- Notes: $4.2B of senior unsecured notes due 2034"}
	axes, err := p.summaryFor(context.Background(), sec, childLines, "")
	if err != nil {
		t.Fatalf("summaryFor: %v", err)
	}
	if axes == nil {
		t.Fatal("summaryFor returned nil axes on happy path")
	}
	if axes.OneLine != "Long-term debt issued by 3M Company and serviced via JPMorgan." {
		t.Errorf("OneLine: got %q", axes.OneLine)
	}
	if want := []string{"debt", "long-term-obligations"}; !sliceEq(axes.Topics, want) {
		t.Errorf("Topics: got %v want %v", axes.Topics, want)
	}
	if want := []string{"3M Company", "JPMorgan", "Q3 2024"}; !sliceEq(axes.Entities, want) {
		t.Errorf("Entities: got %v want %v", axes.Entities, want)
	}
	if want := []string{"$4.2B", "2.8%", "2034"}; !sliceEq(axes.Numbers, want) {
		t.Errorf("Numbers: got %v want %v", axes.Numbers, want)
	}
}

// TestSummaryForEnforcesAxisCaps proves the configured per-axis caps
// trim oversized model output. A misbehaving model that returns 50
// entities must not blow up the prompt budget downstream.
func TestSummaryForEnforcesAxisCaps(t *testing.T) {
	t.Parallel()

	const cannedJSON = `{
        "topics":   ["a","b","c","d","e","f"],
        "entities": ["E1","E2","E3","E4","E5","E6","E7","E8","E9","E10"],
        "numbers":  ["1","2","3","4","5","6","7","8"],
        "one_line": "."
    }`
	m := &llmgate.Mock{Reply: cannedJSON}
	p := &Pipeline{
		LLM:                    m,
		Logger:                 slog.Default(),
		SummaryMaxChars:        4000,
		SummaryModel:           "m",
		SummaryAxesEnabled:     true,
		SummaryAxesMaxTopics:   2,
		SummaryAxesMaxEntities: 3,
		SummaryAxesMaxNumbers:  4,
	}
	axes, err := p.summaryFor(context.Background(), db.Section{ID: "s", Title: "T"}, []string{"- child"}, "")
	if err != nil {
		t.Fatalf("summaryFor: %v", err)
	}
	if axes == nil {
		t.Fatal("nil axes")
	}
	if len(axes.Topics) != 2 {
		t.Errorf("Topics len = %d, want 2", len(axes.Topics))
	}
	if len(axes.Entities) != 3 {
		t.Errorf("Entities len = %d, want 3", len(axes.Entities))
	}
	if len(axes.Numbers) != 4 {
		t.Errorf("Numbers len = %d, want 4", len(axes.Numbers))
	}
}

// TestSummaryForParseFailureUsesRawAsOneLine: when the model returns
// plain text instead of JSON, the structured path retries (up to
// defaultSummaryAxesRetries), and on persistent failure degrades
// gracefully — the section ends up with summary = raw text and
// summary_axes = nil. Ingest does not fail.
func TestSummaryForParseFailureUsesRawAsOneLine(t *testing.T) {
	t.Parallel()
	const raw = "Some long prose the model emitted without JSON wrapping."
	m := &llmgate.Mock{Reply: raw}
	p := &Pipeline{
		LLM:                m,
		Logger:             slog.Default(),
		SummaryMaxChars:    4000,
		SummaryModel:       "m",
		SummaryAxesEnabled: true,
	}
	axes, err := p.summaryFor(context.Background(), db.Section{ID: "s", Title: "T"}, []string{"- child"}, "")
	if err != nil {
		t.Fatalf("summaryFor: %v", err)
	}
	if axes == nil {
		t.Fatal("summaryFor returned nil on parse failure; expected non-nil with OneLine=raw")
	}
	if axes.OneLine != raw {
		t.Errorf("OneLine: got %q want %q", axes.OneLine, raw)
	}
	if len(axes.Topics) != 0 || len(axes.Entities) != 0 || len(axes.Numbers) != 0 {
		t.Errorf("axes lists should be empty on parse failure, got T=%v E=%v N=%v",
			axes.Topics, axes.Entities, axes.Numbers)
	}
}

// TestSummaryForLegacyPathWhenAxesDisabled: with SummaryAxesEnabled=false
// the pipeline must fall back to the pre-2.5 single-sentence prompt
// (no JSON mode) and return axes carrying only OneLine.
func TestSummaryForLegacyPathWhenAxesDisabled(t *testing.T) {
	t.Parallel()
	var sawJSONMode bool
	m := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			if req.JSONMode {
				sawJSONMode = true
			}
			return &llmgate.Response{Content: "A concise sentence."}, nil
		},
	}
	p := &Pipeline{
		LLM:                m,
		Logger:             slog.Default(),
		SummaryMaxChars:    4000,
		SummaryModel:       "m",
		SummaryAxesEnabled: false,
	}
	axes, err := p.summaryFor(context.Background(), db.Section{ID: "s", Title: "T"}, []string{"- child"}, "")
	if err != nil {
		t.Fatalf("summaryFor: %v", err)
	}
	if axes == nil {
		t.Fatal("summaryFor returned nil axes")
	}
	if sawJSONMode {
		t.Errorf("legacy path must not request JSON mode")
	}
	if axes.OneLine != "A concise sentence." {
		t.Errorf("OneLine = %q", axes.OneLine)
	}
	if len(axes.Topics) != 0 || len(axes.Entities) != 0 || len(axes.Numbers) != 0 {
		t.Errorf("legacy path should leave axis lists empty")
	}
}

// TestSummaryForErrNotImplementedFallback: when the LLM is a stub that
// returns ErrNotImplemented (the unit-test default for many setups),
// the structured path must still return a non-nil axes object — with
// OneLine derived from the fallback text — so ingest never gets a
// nil to fail on.
func TestSummaryForErrNotImplementedFallback(t *testing.T) {
	t.Parallel()
	m := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			return nil, llmgate.ErrNotImplemented
		},
	}
	p := &Pipeline{
		LLM:                m,
		Logger:             slog.Default(),
		SummaryMaxChars:    4000,
		SummaryModel:       "m",
		SummaryAxesEnabled: true,
	}
	axes, err := p.summaryFor(context.Background(), db.Section{ID: "s", Title: "My Title"}, []string{"- child"}, "")
	if err != nil {
		t.Fatalf("summaryFor: %v", err)
	}
	if axes == nil {
		t.Fatal("nil axes from ErrNotImplemented")
	}
	if axes.OneLine == "" {
		t.Errorf("OneLine should be non-empty after ErrNotImplemented fallback, got %q", axes.OneLine)
	}
}

// TestSummaryForRetriesParseFailures asserts that a non-JSON response
// is retried per defaultSummaryAxesRetries. We count Complete calls
// — should be 1 (initial) + defaultSummaryAxesRetries = 3 total.
func TestSummaryForRetriesParseFailures(t *testing.T) {
	t.Parallel()
	var calls int
	m := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			calls++
			return &llmgate.Response{Content: "still not JSON"}, nil
		},
	}
	p := &Pipeline{
		LLM:                m,
		Logger:             slog.Default(),
		SummaryMaxChars:    4000,
		SummaryModel:       "m",
		SummaryAxesEnabled: true,
	}
	_, err := p.summaryFor(context.Background(), db.Section{ID: "s", Title: "T"}, []string{"- child"}, "")
	if err != nil {
		t.Fatalf("summaryFor should degrade gracefully: %v", err)
	}
	if want := 1 + defaultSummaryAxesRetries; calls != want {
		t.Errorf("LLM call count: got %d want %d", calls, want)
	}
}

// TestSummaryForTransportErrorReturnsAxesNotError: a transport-level
// failure (network blip) must still degrade gracefully — ingest is
// fault-tolerant about summarization, so we want a fallback axes
// object back, not a propagated error.
func TestSummaryForTransportErrorReturnsAxesNotError(t *testing.T) {
	t.Parallel()
	boom := errors.New("network error")
	m := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			return nil, boom
		},
	}
	p := &Pipeline{
		LLM:                m,
		Logger:             slog.Default(),
		SummaryMaxChars:    4000,
		SummaryModel:       "m",
		SummaryAxesEnabled: true,
	}
	axes, err := p.summaryFor(context.Background(), db.Section{ID: "s", Title: "T"}, []string{"- child"}, "")
	if err != nil {
		t.Fatalf("summaryFor should swallow transport errors: %v", err)
	}
	if axes == nil {
		t.Fatal("nil axes from transport error")
	}
	// OneLine populated via fallback so retrieval still has signal.
	if axes.OneLine == "" {
		t.Error("OneLine should be non-empty after transport-error fallback")
	}
}

// TestParseSummaryAxesTolerantToCodeFences mirrors the
// ParseSelection / parseHyDEResponse contracts — JSON-mode models
// occasionally wrap responses in ```json fences or prose, and the
// parser must look through both.
func TestParseSummaryAxesTolerantToCodeFences(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"one_line":"x"}`,
		"```json\n{\"one_line\":\"x\"}\n```",
		"Sure! Here you go: {\"one_line\":\"x\"} hope this helps.",
	}
	for _, c := range cases {
		axes, err := parseSummaryAxes(c)
		if err != nil {
			t.Errorf("parse failed for %q: %v", c, err)
			continue
		}
		if axes == nil || axes.OneLine != "x" {
			t.Errorf("parse(%q) = %+v, want OneLine=x", c, axes)
		}
	}
}

// TestParseSummaryAxesRejectsGarbage: non-JSON input must error so
// the retry loop fires (rather than silently producing an empty
// SummaryAxes).
func TestParseSummaryAxesRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := parseSummaryAxes("not json at all"); err == nil {
		t.Error("expected parse error for non-JSON input")
	}
	if _, err := parseSummaryAxes(""); err == nil {
		t.Error("expected parse error for empty input")
	}
}

// TestParseSummaryAxesTrimsBlanks: blank/whitespace-only strings in
// the array axes get dropped at parse time so a sloppy model that
// returns ["debt", "", "  "] doesn't pollute the retrieval prompt.
func TestParseSummaryAxesTrimsBlanks(t *testing.T) {
	t.Parallel()
	in := `{"topics":["debt","","  "], "entities":["3M",""], "numbers":[" "], "one_line":"  hello  "}`
	axes, err := parseSummaryAxes(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(axes.Topics) != 1 || axes.Topics[0] != "debt" {
		t.Errorf("Topics: got %v", axes.Topics)
	}
	if len(axes.Entities) != 1 || axes.Entities[0] != "3M" {
		t.Errorf("Entities: got %v", axes.Entities)
	}
	if len(axes.Numbers) != 0 {
		t.Errorf("Numbers: should be empty after trim, got %v", axes.Numbers)
	}
	if axes.OneLine != "hello" {
		t.Errorf("OneLine: got %q, want trimmed 'hello'", axes.OneLine)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
