package retrieval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// contains is a tiny shim so we don't have to litter assertions with
// strings.Contains; the external _test package cannot share span_test.go's
// internal helper.
func contains(s, sub string) bool { return strings.Contains(s, sub) }

// rerankMock is a minimal llmgate client that returns scripted replies
// in order. Each Complete() call advances the counter; when the
// scripted replies are exhausted the last reply is reused so a
// single-element script behaves like a fixed response.
//
// Kept separate from planner/single_pass mocks because the re-rank
// schema is distinct and the test surface is small enough that a
// dedicated mock keeps the assertions simple.
type rerankMock struct {
	mu      sync.Mutex
	replies []string
	err     error

	calls   int32
	prompts []string
}

func (m *rerankMock) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	atomic.AddInt32(&m.calls, 1)
	for _, msg := range req.Messages {
		if msg.Role == llmgate.RoleUser {
			m.prompts = append(m.prompts, msg.Content)
		}
	}
	if m.err != nil {
		return nil, m.err
	}
	if len(m.replies) == 0 {
		return &llmgate.Response{}, nil
	}
	idx := int(atomic.LoadInt32(&m.calls)) - 1
	if idx >= len(m.replies) {
		idx = len(m.replies) - 1
	}
	return &llmgate.Response{
		Content: m.replies[idx],
		Usage: llmgate.Usage{
			InputTokens:  100,
			OutputTokens: 30,
			TotalTokens:  130,
			CostUSD:      0.0003,
		},
	}, nil
}

func (m *rerankMock) CountTokens(ctx context.Context, s string) (int, error) {
	return len(s) / 4, nil
}

// scoreReply marshals a list of (id, score, reason) tuples into the
// re-rank JSON envelope.
func scoreReply(items ...rerankReplyItem) string {
	type payload struct {
		Scores []rerankReplyItem `json:"scores"`
	}
	raw, _ := json.Marshal(payload{Scores: items})
	return string(raw)
}

type rerankReplyItem struct {
	ID     string  `json:"id"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason,omitempty"`
}

// sampleCandidates builds a small candidate list used by most tests.
// "sec-1" / "sec-2" / "sec-3" — short stable IDs so assertions are
// readable.
func sampleCandidates() []retrieval.SectionContent {
	return []retrieval.SectionContent{
		{ID: tree.SectionID("sec-1"), Title: "Long-Term Debt", Content: "The company reported long-term debt of $4.2B as of Q4 2024."},
		{ID: tree.SectionID("sec-2"), Title: "Revenue Breakdown", Content: "Apple's fiscal 2023 revenue was $383.3B, down 2.8% YoY."},
		{ID: tree.SectionID("sec-3"), Title: "Risk Factors", Content: "Foreign currency translation may impact future revenue."},
	}
}

// TestReRanker_HappyPath: model returns reordered scores, output is
// sorted descending by score.
func TestReRanker_HappyPath(t *testing.T) {
	t.Parallel()
	m := &rerankMock{
		replies: []string{
			// sec-2 is the most relevant (92), sec-3 next (45), sec-1 last (10).
			// Strategy returned them in 1/2/3 order — re-rank should flip it.
			scoreReply(
				rerankReplyItem{ID: "sec-1", Score: 10, Reason: "long-term debt not relevant to revenue query"},
				rerankReplyItem{ID: "sec-2", Score: 92, Reason: "directly states fiscal 2023 revenue"},
				rerankReplyItem{ID: "sec-3", Score: 45, Reason: "tangential mention of revenue"},
			),
		},
	}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")

	got, usage, err := r.ReRank(context.Background(), "What was Apple's fiscal 2023 revenue?", sampleCandidates())
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	wantOrder := []tree.SectionID{"sec-2", "sec-3", "sec-1"}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("got[%d].ID = %q, want %q (full order: %+v)", i, got[i].ID, w, got)
		}
	}
	if got[0].Score != 92 {
		t.Errorf("top score = %v, want 92", got[0].Score)
	}
	if got[0].Reason == "" {
		t.Error("top entry should carry a reason from the model")
	}
	if usage.LLMCalls != 1 {
		t.Errorf("Usage.LLMCalls = %d, want 1", usage.LLMCalls)
	}
	if usage.CostUSD <= 0 {
		t.Errorf("Usage.CostUSD = %v, want > 0", usage.CostUSD)
	}
}

// TestReRanker_EmptyInput: nil/empty candidate list short-circuits
// without an LLM call.
func TestReRanker_EmptyInput(t *testing.T) {
	t.Parallel()
	m := &rerankMock{replies: []string{`{"scores":[]}`}}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")

	got, usage, err := r.ReRank(context.Background(), "irrelevant", nil)
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	if got != nil {
		t.Errorf("empty input should return nil slice, got %+v", got)
	}
	if usage.LLMCalls != 0 {
		t.Errorf("empty input should issue no LLM calls, got %d", usage.LLMCalls)
	}
	if c := atomic.LoadInt32(&m.calls); c != 0 {
		t.Errorf("empty input must NOT call the LLM, got %d calls", c)
	}
}

// TestReRanker_LLMFailure: transport error bubbles up, input order is
// preserved with score=0 so the caller never loses a candidate.
func TestReRanker_LLMFailure(t *testing.T) {
	t.Parallel()
	m := &rerankMock{err: errors.New("provider 500")}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")

	cands := sampleCandidates()
	got, _, err := r.ReRank(context.Background(), "any query", cands)
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if len(got) != len(cands) {
		t.Fatalf("LLM failure must preserve all candidates: got %d, want %d", len(got), len(cands))
	}
	for i, c := range cands {
		if got[i].ID != c.ID {
			t.Errorf("got[%d].ID = %q, want %q (preserved input order)", i, got[i].ID, c.ID)
		}
		if got[i].Score != 0 {
			t.Errorf("got[%d].Score = %v, want 0 on transport failure", i, got[i].Score)
		}
	}
}

// TestReRanker_BadJSONExhaustsRetries: when all retry attempts return
// un-parseable JSON, returns input order + nil error (graceful
// degradation, matching runSelectionWithRetry behaviour).
func TestReRanker_BadJSONExhaustsRetries(t *testing.T) {
	t.Parallel()
	m := &rerankMock{
		replies: []string{
			"sorry, here's some prose instead of JSON",
			"still talking, not JSON",
			"and one more time",
		},
	}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")
	r.MaxRetries = 2 // 1 initial + 2 retries = 3 attempts

	cands := sampleCandidates()
	got, usage, err := r.ReRank(context.Background(), "any query", cands)
	if err != nil {
		t.Fatalf("parse-only failure must return nil error, got %v", err)
	}
	if c := atomic.LoadInt32(&m.calls); c != 3 {
		t.Errorf("expected 3 LLM attempts (1 + 2 retries), got %d", c)
	}
	if usage.LLMCalls != 3 {
		t.Errorf("usage.LLMCalls = %d, want 3 (all attempts counted)", usage.LLMCalls)
	}
	if len(got) != len(cands) {
		t.Fatalf("parse failure must preserve all candidates: got %d", len(got))
	}
	for i, c := range cands {
		if got[i].ID != c.ID {
			t.Errorf("got[%d].ID = %q, want %q (input order)", i, got[i].ID, c.ID)
		}
		if got[i].Score != 0 {
			t.Errorf("got[%d].Score = %v, want 0 on parse failure", i, got[i].Score)
		}
	}
}

// TestReRanker_BadJSONThenSuccess: a single bad reply followed by a
// good one returns the parsed scores.
func TestReRanker_BadJSONThenSuccess(t *testing.T) {
	t.Parallel()
	m := &rerankMock{
		replies: []string{
			"not json",
			scoreReply(
				rerankReplyItem{ID: "sec-1", Score: 50},
				rerankReplyItem{ID: "sec-2", Score: 80},
				rerankReplyItem{ID: "sec-3", Score: 20},
			),
		},
	}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")
	got, usage, err := r.ReRank(context.Background(), "Q", sampleCandidates())
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	if usage.LLMCalls != 2 {
		t.Errorf("LLMCalls = %d, want 2 (1 failed + 1 ok)", usage.LLMCalls)
	}
	if got[0].ID != "sec-2" {
		t.Errorf("top = %q, want sec-2 (highest score)", got[0].ID)
	}
}

// TestReRanker_UnknownIDDropped: when the model invents an ID, it is
// silently dropped from the output. The known IDs still surface.
func TestReRanker_UnknownIDDropped(t *testing.T) {
	t.Parallel()
	m := &rerankMock{
		replies: []string{
			scoreReply(
				rerankReplyItem{ID: "sec-1", Score: 30},
				rerankReplyItem{ID: "sec-2", Score: 70},
				rerankReplyItem{ID: "sec-3", Score: 50},
				rerankReplyItem{ID: "sec-bogus", Score: 99, Reason: "hallucinated"},
			),
		},
	}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")
	got, _, err := r.ReRank(context.Background(), "Q", sampleCandidates())
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (bogus ID dropped)", len(got))
	}
	for _, s := range got {
		if s.ID == "sec-bogus" {
			t.Errorf("hallucinated ID leaked into output: %+v", s)
		}
	}
	if got[0].ID != "sec-2" {
		t.Errorf("top = %q, want sec-2", got[0].ID)
	}
}

// TestReRanker_MissingIDsScoreZero: input IDs the model didn't score
// appear at the bottom with score=0.
func TestReRanker_MissingIDsScoreZero(t *testing.T) {
	t.Parallel()
	m := &rerankMock{
		replies: []string{
			// Model only scored sec-2; sec-1 and sec-3 are missing.
			scoreReply(rerankReplyItem{ID: "sec-2", Score: 88}),
		},
	}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")
	got, _, err := r.ReRank(context.Background(), "Q", sampleCandidates())
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (missing IDs must still surface)", len(got))
	}
	if got[0].ID != "sec-2" || got[0].Score != 88 {
		t.Errorf("top = %+v, want sec-2 / 88", got[0])
	}
	// Missing IDs come back with score=0 in input order. sec-1 was input
	// position 0 and sec-3 was input position 2, so we expect sec-1 then sec-3.
	if got[1].ID != "sec-1" || got[1].Score != 0 {
		t.Errorf("got[1] = %+v, want sec-1 / 0", got[1])
	}
	if got[2].ID != "sec-3" || got[2].Score != 0 {
		t.Errorf("got[2] = %+v, want sec-3 / 0", got[2])
	}
}

// TestReRanker_DuplicateIDsInResponse: when the model returns the same
// ID twice the first wins and the duplicate is dropped.
func TestReRanker_DuplicateIDsInResponse(t *testing.T) {
	t.Parallel()
	m := &rerankMock{
		replies: []string{
			scoreReply(
				rerankReplyItem{ID: "sec-1", Score: 60, Reason: "first"},
				rerankReplyItem{ID: "sec-1", Score: 10, Reason: "duplicate"},
				rerankReplyItem{ID: "sec-2", Score: 30},
				rerankReplyItem{ID: "sec-3", Score: 20},
			),
		},
	}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")
	got, _, err := r.ReRank(context.Background(), "Q", sampleCandidates())
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if got[0].ID != "sec-1" || got[0].Score != 60 || got[0].Reason != "first" {
		t.Errorf("expected first occurrence to win, got %+v", got[0])
	}
}

// TestReRanker_NegativeScoreClamped: a negative score from the model
// is clamped to 0 by ParseReRank.
func TestReRanker_NegativeScoreClamped(t *testing.T) {
	t.Parallel()
	m := &rerankMock{
		replies: []string{
			scoreReply(
				rerankReplyItem{ID: "sec-1", Score: -5},
				rerankReplyItem{ID: "sec-2", Score: 30},
				rerankReplyItem{ID: "sec-3", Score: 0},
			),
		},
	}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")
	got, _, err := r.ReRank(context.Background(), "Q", sampleCandidates())
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	// Top should be sec-2 (30). sec-1 with score=-5 is clamped to 0; it
	// will tie with sec-3 (0) and the stable-sort tiebreak puts sec-1
	// before sec-3 since that's the input order.
	if got[0].ID != "sec-2" {
		t.Errorf("top = %q, want sec-2", got[0].ID)
	}
	for _, s := range got {
		if s.Score < 0 {
			t.Errorf("negative score leaked through: %+v", s)
		}
	}
}

// TestReRanker_PromptIncludesContent: the prompt actually carries the
// candidate content (otherwise re-rank is back to title-only).
func TestReRanker_PromptIncludesContent(t *testing.T) {
	t.Parallel()
	m := &rerankMock{
		replies: []string{scoreReply(
			rerankReplyItem{ID: "sec-1", Score: 50},
			rerankReplyItem{ID: "sec-2", Score: 50},
			rerankReplyItem{ID: "sec-3", Score: 50},
		)},
	}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")
	_, _, err := r.ReRank(context.Background(), "What was Apple's fiscal 2023 revenue?", sampleCandidates())
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	if len(m.prompts) != 1 {
		t.Fatalf("want 1 captured user prompt, got %d", len(m.prompts))
	}
	prompt := m.prompts[0]
	// All three IDs surfaced.
	for _, id := range []string{"sec-1", "sec-2", "sec-3"} {
		if !contains(prompt, "["+id+"]") {
			t.Errorf("prompt missing ID marker %q", id)
		}
	}
	// Content excerpt for sec-2 must appear (the prompt is title + body).
	if !contains(prompt, "Apple's fiscal 2023 revenue was $383.3B") {
		t.Error("prompt missing sec-2 content excerpt")
	}
	if !contains(prompt, "What was Apple's fiscal 2023 revenue?") {
		t.Error("prompt missing user query")
	}
}

// TestReRanker_MaxContentCharsTruncates: when content exceeds the cap
// the prompt carries a truncated excerpt with an ellipsis.
func TestReRanker_MaxContentCharsTruncates(t *testing.T) {
	t.Parallel()
	m := &rerankMock{replies: []string{scoreReply(rerankReplyItem{ID: "sec-1", Score: 50})}}
	r := retrieval.NewReRanker(m, "gemini-2.5-flash")
	r.MaxContentChars = 20

	longContent := "AAAAAAAAAA BBBBBBBBBB CCCCCCCCCC DDDDDDDDDD EEEEEEEEEE"
	_, _, err := r.ReRank(context.Background(), "Q", []retrieval.SectionContent{
		{ID: tree.SectionID("sec-1"), Title: "T", Content: longContent},
	})
	if err != nil {
		t.Fatalf("ReRank: %v", err)
	}
	prompt := m.prompts[0]
	if !contains(prompt, "AAAAAAAAAA BBBBBBBBB") {
		t.Errorf("prompt missing first 20 chars of long content")
	}
	if !contains(prompt, "…") {
		t.Error("prompt missing ellipsis marker for truncation")
	}
	if contains(prompt, "EEEEEEEEEE") {
		t.Error("prompt unexpectedly contained tail of long content")
	}
}

// TestReRanker_NilLLMNoOp: a re-ranker with a nil LLM client returns
// input order without panicking. This lets server wiring pass a stub
// for the disabled case.
func TestReRanker_NilLLMNoOp(t *testing.T) {
	t.Parallel()
	r := &retrieval.ReRanker{Model: "any"}
	got, usage, err := r.ReRank(context.Background(), "Q", sampleCandidates())
	if err != nil {
		t.Fatalf("ReRank with nil LLM: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("nil LLM should preserve input order, got %d entries", len(got))
	}
	if usage.LLMCalls != 0 {
		t.Errorf("nil LLM should issue no calls, got %d", usage.LLMCalls)
	}
}

// TestReRanker_NilReRankerNoOp: a nil *ReRanker is safe — callers can
// pass nil when re-rank is disabled.
func TestReRanker_NilReRankerNoOp(t *testing.T) {
	t.Parallel()
	var r *retrieval.ReRanker
	got, usage, err := r.ReRank(context.Background(), "Q", sampleCandidates())
	if err != nil {
		t.Fatalf("nil ReRanker: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("nil ReRanker should preserve input order, got %d entries", len(got))
	}
	if usage.LLMCalls != 0 {
		t.Errorf("nil ReRanker should issue no calls, got %d", usage.LLMCalls)
	}
}

// TestParseReRank_CodeFence: tolerant parsing matches the other
// JSON-mode parsers in this package.
func TestParseReRank_CodeFence(t *testing.T) {
	t.Parallel()
	raw := "```json\n" + scoreReply(rerankReplyItem{ID: "sec-1", Score: 75}) + "\n```"
	got, err := retrieval.ParseReRank(raw)
	if err != nil {
		t.Fatalf("ParseReRank: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sec-1" || got[0].Score != 75 {
		t.Errorf("got %+v", got)
	}
}

// TestParseReRank_LeadingProse: leading prose ahead of the JSON object
// is stripped.
func TestParseReRank_LeadingProse(t *testing.T) {
	t.Parallel()
	raw := "Sure, here are the scores: " + scoreReply(rerankReplyItem{ID: "sec-1", Score: 10})
	got, err := retrieval.ParseReRank(raw)
	if err != nil {
		t.Fatalf("ParseReRank: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %+v", got)
	}
}

// TestParseReRank_Empty: empty / whitespace-only responses error so
// retry can fire.
func TestParseReRank_Empty(t *testing.T) {
	t.Parallel()
	if _, err := retrieval.ParseReRank(""); err == nil {
		t.Error("empty input should parse-error so retry fires")
	}
	if _, err := retrieval.ParseReRank("   \n\n  "); err == nil {
		t.Error("whitespace input should parse-error")
	}
}
