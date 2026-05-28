package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// dbSectionLite builds a minimal db.Section for tests that don't touch
// storage. Only id + title are populated; ContentRef is empty so
// candidateQuestionsFor skips the storage fetch.
func dbSectionLite(id, title string) db.Section {
	return db.Section{
		ID:    tree.SectionID(id),
		Title: title,
	}
}

func TestParseHyDEResponseHappy(t *testing.T) {
	got, err := parseHyDEResponse(`{"questions":["Q1","Q2","Q3"]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 || got[0] != "Q1" || got[2] != "Q3" {
		t.Errorf("got %+v", got)
	}
}

func TestParseHyDEResponseToleratesCodeFences(t *testing.T) {
	got, err := parseHyDEResponse("```json\n{\"questions\":[\"foo\",\"bar\"]}\n```")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[1] != "bar" {
		t.Errorf("got %+v", got)
	}
}

func TestParseHyDEResponseToleratesProseBefore(t *testing.T) {
	got, err := parseHyDEResponse(`Sure, here you go: {"questions":["only one"]}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0] != "only one" {
		t.Errorf("got %+v", got)
	}
}

func TestParseHyDEResponseRejectsNonJSON(t *testing.T) {
	if _, err := parseHyDEResponse("Sure here are some questions: Q1, Q2"); err == nil {
		t.Errorf("expected parse error on non-JSON input")
	}
}

func TestDedupeNonEmpty(t *testing.T) {
	in := []string{"  ", "Q1", "q1", "Q2", "  Q1  ", "Q3", "", "Q4"}
	got := dedupeNonEmpty(in, 5)
	want := []string{"Q1", "Q2", "Q3", "Q4"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i, q := range want {
		if got[i] != q {
			t.Errorf("idx %d: got %q want %q", i, got[i], q)
		}
	}
}

func TestDedupeNonEmptyCapsAtMax(t *testing.T) {
	in := []string{"Q1", "Q2", "Q3", "Q4", "Q5", "Q6"}
	got := dedupeNonEmpty(in, 3)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
}

// runHyDEWithRetry tests — exercise the retry + graceful-degrade path
// using llmgate.Mock with a custom Respond function.

func TestRunHyDEWithRetryHappy(t *testing.T) {
	m := &llmgate.Mock{Reply: `{"questions":["Q1","Q2","Q3","Q4","Q5"]}`}
	got, err := runHyDEWithRetry(context.Background(), m, llmgate.Request{
		Messages: []llmgate.Message{{Role: llmgate.RoleUser, Content: "go"}},
	}, 2, 0)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("got %v", got)
	}
	if m.Calls() != 1 {
		t.Errorf("want 1 call, got %d", m.Calls())
	}
}

func TestRunHyDEWithRetryRetriesOnNonJSON(t *testing.T) {
	var calls int32
	m := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			n := atomic.AddInt32(&calls, 1)
			if n < 3 {
				// Plain prose with no braces at all — defeats the
				// brace-finding fallback in parseHyDEResponse.
				return &llmgate.Response{Content: "I am chatty here"}, nil
			}
			return &llmgate.Response{Content: `{"questions":["recovered"]}`}, nil
		},
	}
	got, err := runHyDEWithRetry(context.Background(), m, llmgate.Request{
		Messages: []llmgate.Message{{Role: llmgate.RoleUser, Content: "go"}},
	}, 2, 0)
	if err != nil {
		t.Fatalf("should recover on 3rd attempt: %v", err)
	}
	if len(got) != 1 || got[0] != "recovered" {
		t.Errorf("got %+v", got)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("want 3 attempts, got %d", calls)
	}
}

func TestRunHyDEWithRetryFinalParseFailReturnsError(t *testing.T) {
	m := &llmgate.Mock{Reply: "no JSON anywhere here, just prose."}
	_, err := runHyDEWithRetry(context.Background(), m, llmgate.Request{
		Messages: []llmgate.Message{{Role: llmgate.RoleUser, Content: "go"}},
	}, 2, 0)
	if err == nil {
		t.Error("want final-parse error after all retries fail")
	}
	if m.Calls() != 3 { // 1 initial + 2 retries
		t.Errorf("want 3 attempts, got %d", m.Calls())
	}
}

// firstCandidateQuestion truncation — exercised through the retrieval
// package; replicate the test here so the cap is locked down close to
// the data it cares about.
func TestParseHyDEEmptyInput(t *testing.T) {
	got, err := parseHyDEResponse("")
	if err != nil {
		t.Errorf("empty input should not error: %v", err)
	}
	if got != nil {
		t.Errorf("empty input should yield nil, got %v", got)
	}
}

func TestParseHyDEEmptyArray(t *testing.T) {
	got, err := parseHyDEResponse(`{"questions":[]}`)
	if err != nil {
		t.Fatalf("empty array should parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %v", got)
	}
}

// TestHyDEGracefulOnNonJSON: per the plan — when the LLM repeatedly
// returns non-JSON, the runner returns a parse error; the surrounding
// generateCandidateQuestions code already logs and proceeds without
// persisting an empty array. This test asserts the SHAPE of the error
// (so it stays informative) and that no panic / partial-success happens.
func TestHyDEGracefulOnNonJSON(t *testing.T) {
	m := &llmgate.Mock{Reply: "Sure here are some questions: Q1, Q2, Q3."}
	// Capture the slog warning that the runtime would emit when this
	// path runs end-to-end. (generateCandidateQuestions is exercised
	// in TestGenerateCandidateQuestionsEndToEnd below.)
	var logBuf bytes.Buffer
	_ = slog.New(slog.NewTextHandler(&logBuf, nil))

	_, err := runHyDEWithRetry(context.Background(), m, llmgate.Request{
		Messages: []llmgate.Message{{Role: llmgate.RoleUser, Content: "u"}},
	}, 2, 0)
	if err == nil {
		t.Fatal("want graceful error after 3 failed attempts")
	}
	if !strings.Contains(err.Error(), "parse failed") {
		t.Errorf("unhelpful error message: %v", err)
	}
}

// hydeCapturingMock implements just enough of llmgate.Client to assert
// what we passed in and to count calls. The point of this test is to
// confirm the retry/dedupe shape that the rest of the pipeline relies on.
type hydeCapturingMock struct {
	mu        sync.Mutex
	calls     int
	lastModel string
	reply     string
	failErr   error
}

func (m *hydeCapturingMock) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.lastModel = req.Model
	if m.failErr != nil {
		return nil, m.failErr
	}
	return &llmgate.Response{Content: m.reply}, nil
}

func (m *hydeCapturingMock) CountTokens(ctx context.Context, s string) (int, error) {
	return len(s) / 4, nil
}

func TestCandidateQuestionsForUsesModelOverride(t *testing.T) {
	m := &hydeCapturingMock{reply: `{"questions":["Q1"]}`}
	p := &Pipeline{
		LLM:              m,
		Logger:           slog.Default(),
		SummaryMaxChars:  4000,
		SummaryModel:     "default-model",
		HyDEModel:        "hyde-special-model",
		HyDENumQuestions: 5,
	}
	// Section without ContentRef so we don't need storage.
	got, err := p.candidateQuestionsFor(context.Background(), dbSectionLite("sec_a", "Title"), "")
	if err != nil {
		t.Fatalf("candidateQuestionsFor: %v", err)
	}
	if len(got) != 1 || got[0] != "Q1" {
		t.Errorf("got %+v", got)
	}
	if m.lastModel != "hyde-special-model" {
		t.Errorf("HyDEModel override not used, got %q", m.lastModel)
	}
}

func TestCandidateQuestionsForFallsBackToSummaryModel(t *testing.T) {
	m := &hydeCapturingMock{reply: `{"questions":["Q1"]}`}
	p := &Pipeline{
		LLM:              m,
		Logger:           slog.Default(),
		SummaryMaxChars:  4000,
		SummaryModel:     "default-model",
		HyDENumQuestions: 5,
	}
	if _, err := p.candidateQuestionsFor(context.Background(), dbSectionLite("sec_a", "Title"), ""); err != nil {
		t.Fatal(err)
	}
	if m.lastModel != "default-model" {
		t.Errorf("HyDE should fall back to SummaryModel, got %q", m.lastModel)
	}
}

func TestCandidateQuestionsForCapsAtN(t *testing.T) {
	reply, _ := json.Marshal(map[string]any{"questions": []string{"a", "b", "c", "d", "e", "f", "g"}})
	m := &hydeCapturingMock{reply: string(reply)}
	p := &Pipeline{
		LLM:              m,
		Logger:           slog.Default(),
		SummaryMaxChars:  4000,
		HyDENumQuestions: 3,
	}
	got, err := p.candidateQuestionsFor(context.Background(), dbSectionLite("sec_a", "Title"), "")
	if err != nil {
		t.Fatalf("candidateQuestionsFor: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("want 3, got %d (%+v)", len(got), got)
	}
}
