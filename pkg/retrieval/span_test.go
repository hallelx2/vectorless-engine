package retrieval

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/llmgate"
)

// spanMockLLM is a minimal LLM stub for span-extractor tests. The
// retrieval_test.go file uses an external-package mock; we need an
// internal one to exercise locateQuote / parseSpanResponse directly.
type spanMockLLM struct {
	reply string
	calls int32
}

func (m *spanMockLLM) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	atomic.AddInt32(&m.calls, 1)
	return &llmgate.Response{Content: m.reply}, nil
}

func (m *spanMockLLM) CountTokens(ctx context.Context, s string) (int, error) {
	return len(s) / 4, nil
}

func TestSpanExtractor_VerbatimMatch(t *testing.T) {
	content := "Apple Inc. reported revenue of $383.3 billion for fiscal 2023, up 2.8% year over year. " +
		"The iPhone segment generated $200.6 billion of that total."
	query := "What was Apple's fiscal 2023 revenue?"

	m := &spanMockLLM{reply: `{"found":true,"quote":"revenue of $383.3 billion for fiscal 2023"}`}
	e := NewSpanExtractor(m, "gemini-2.5-flash")

	span, usage, err := e.Extract(context.Background(), content, query)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if span == nil {
		t.Fatalf("expected non-nil span")
	}
	if span.Text != "revenue of $383.3 billion for fiscal 2023" {
		t.Errorf("text = %q", span.Text)
	}
	if span.Start <= 0 || span.End <= span.Start {
		t.Errorf("offsets = (%d, %d)", span.Start, span.End)
	}
	if got := content[span.Start:span.End]; got != span.Text {
		t.Errorf("content[Start:End] = %q, want %q", got, span.Text)
	}
	if usage.LLMCalls != 1 {
		t.Errorf("usage.LLMCalls = %d, want 1", usage.LLMCalls)
	}
}

func TestSpanExtractor_NotFound(t *testing.T) {
	content := "This section is about unrelated topics."
	m := &spanMockLLM{reply: `{"found":false,"quote":""}`}
	e := NewSpanExtractor(m, "gemini-2.5-flash")

	span, usage, err := e.Extract(context.Background(), content, "Q")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if span != nil {
		t.Errorf("expected nil span, got %+v", span)
	}
	if usage.LLMCalls != 1 {
		t.Errorf("usage.LLMCalls = %d, want 1", usage.LLMCalls)
	}
}

func TestSpanExtractor_ParaphraseFallsBackToWhitespace(t *testing.T) {
	// The original has weird whitespace (a newline mid-sentence + extra
	// spaces) but the model returns a normalised version. We should
	// still locate it.
	content := "Apple Inc. reported revenue of\n  $383.3   billion for fiscal 2023, up 2.8% year over year."
	m := &spanMockLLM{reply: `{"found":true,"quote":"revenue of $383.3 billion for fiscal 2023"}`}
	e := NewSpanExtractor(m, "gemini-2.5-flash")

	span, _, err := e.Extract(context.Background(), content, "revenue?")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if span == nil {
		t.Fatalf("expected non-nil span via whitespace match")
	}
	if span.Start < 0 || span.End < 0 {
		t.Errorf("expected resolved offsets via WS-normalised match, got (%d, %d)", span.Start, span.End)
	}
}

func TestSpanExtractor_QuoteNotInContent(t *testing.T) {
	// Model invents text not present anywhere — sentinel offsets, but
	// span.Text still surfaces what the model said so callers can flag.
	content := "Plain content with no apple references at all."
	m := &spanMockLLM{reply: `{"found":true,"quote":"hallucinated quote that does not appear"}`}
	e := NewSpanExtractor(m, "gemini-2.5-flash")

	span, _, err := e.Extract(context.Background(), content, "Q")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if span == nil {
		t.Fatalf("expected non-nil span even with bad quote")
	}
	if span.Start != -1 || span.End != -1 {
		t.Errorf("expected sentinel offsets (-1,-1) for hallucinated quote, got (%d,%d)", span.Start, span.End)
	}
	if span.Text == "" {
		t.Errorf("expected text preserved")
	}
}

func TestSpanExtractor_EmptyInput(t *testing.T) {
	m := &spanMockLLM{reply: `{"found":true,"quote":"x"}`}
	e := NewSpanExtractor(m, "gemini-2.5-flash")
	if span, _, _ := e.Extract(context.Background(), "", "Q"); span != nil {
		t.Errorf("empty content should yield nil span without an LLM call")
	}
	if span, _, _ := e.Extract(context.Background(), "content", ""); span != nil {
		t.Errorf("empty query should yield nil span without an LLM call")
	}
}

func TestParseSpanResponse_CodeFence(t *testing.T) {
	raw := "```json\n{\"found\":true,\"quote\":\"hello\"}\n```"
	q, found, err := parseSpanResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !found || q != "hello" {
		t.Errorf("got (%q, %v)", q, found)
	}
}

func TestParseSpanResponse_LeadingProse(t *testing.T) {
	raw := "Sure, here is the span: {\"found\":true,\"quote\":\"x\"}"
	q, found, _ := parseSpanResponse(raw)
	if !found || q != "x" {
		t.Errorf("leading-prose parse failed: %q, %v", q, found)
	}
}

func TestLocateQuote_Exact(t *testing.T) {
	c := "alpha beta gamma"
	s, e := locateQuote(c, "beta")
	if s != 6 || e != 10 {
		t.Errorf("got (%d,%d), want (6,10)", s, e)
	}
}

func TestLocateQuote_WhitespaceNormalised(t *testing.T) {
	c := "alpha\n\n  beta   gamma"
	s, e := locateQuote(c, "beta gamma")
	if s < 0 || e <= s {
		t.Fatalf("got (%d,%d) — expected resolved offsets", s, e)
	}
	if !contains(c[s:e], "beta") || !contains(c[s:e], "gamma") {
		t.Errorf("located span %q does not contain target words", c[s:e])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
