package retrieval_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// scriptedLLM returns a sequence of canned responses, one per Complete
// call, exactly as the agentic loop needs for deterministic tests. If
// the script is exhausted, the LLM returns the loopReply value (used
// by the hop-cap test to simulate a model that never decides to
// terminate).
type scriptedLLM struct {
	replies   []string
	loopReply string

	calls int32

	mu          sync.Mutex
	lastPrompts []string
}

func (s *scriptedLLM) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	i := int(atomic.AddInt32(&s.calls, 1)) - 1

	// Capture the most recent user message for later assertions.
	var userMsg string
	for _, msg := range req.Messages {
		if msg.Role == llmgate.RoleUser {
			userMsg = msg.Content
		}
	}
	s.mu.Lock()
	s.lastPrompts = append(s.lastPrompts, userMsg)
	s.mu.Unlock()

	if i < len(s.replies) {
		return &llmgate.Response{Content: s.replies[i]}, nil
	}
	if s.loopReply != "" {
		return &llmgate.Response{Content: s.loopReply}, nil
	}
	// Exhausted with no loopReply — surface as an error so the test
	// notices it under-scripted instead of silently hanging.
	return nil, errors.New("scriptedLLM: replies exhausted")
}

func (s *scriptedLLM) CountTokens(ctx context.Context, t string) (int, error) {
	return len(t) / 4, nil
}

// mapFetcher is an in-memory ContentFetcher backed by a map. The
// agentic strategy only needs Get; we don't bother modelling errors
// here because the tests use real refs.
type mapFetcher struct{ data map[string]string }

func (m mapFetcher) Get(ctx context.Context, ref string) ([]byte, error) {
	v, ok := m.data[ref]
	if !ok {
		return nil, errors.New("not found")
	}
	return []byte(v), nil
}

// buildAgenticTree constructs a 3-level test tree:
//
//	sec_root → [sec_a, sec_b]
//	  sec_a → [sec_a1 (leaf, ref=a1_ref), sec_a2 (leaf, ref=a2_ref)]
//	  sec_b → [sec_b1 (leaf, ref=b1_ref)]
//
// Enough depth to exercise expand and read in sequence.
func buildAgenticTree() *tree.Tree {
	a1 := &tree.Section{ID: "sec_a1", ParentID: "sec_a", Title: "Install", Summary: "install steps", ContentRef: "a1_ref"}
	a2 := &tree.Section{ID: "sec_a2", ParentID: "sec_a", Title: "Config", Summary: "config keys", ContentRef: "a2_ref"}
	b1 := &tree.Section{ID: "sec_b1", ParentID: "sec_b", Title: "Querying", Summary: "how to query", ContentRef: "b1_ref"}
	a := &tree.Section{ID: "sec_a", ParentID: "sec_root", Title: "Setup", Summary: "setup section", Children: []*tree.Section{a1, a2}}
	b := &tree.Section{ID: "sec_b", ParentID: "sec_root", Title: "Usage", Summary: "usage section", Children: []*tree.Section{b1}}
	root := &tree.Section{ID: "sec_root", Title: "Atlas", Children: []*tree.Section{a, b}}
	return &tree.Tree{DocumentID: "doc_x", Title: "Atlas", Root: root}
}

func TestAgenticHappyPath(t *testing.T) {
	t.Parallel()

	tr := buildAgenticTree()
	llm := &scriptedLLM{
		replies: []string{
			`{"action":"expand","section_id":"sec_a"}`,
			`{"action":"read","section_id":"sec_a1"}`,
			`{"action":"done","picked_ids":["sec_a1"],"reasoning":"install matches the query"}`,
		},
	}
	fetcher := mapFetcher{data: map[string]string{
		"a1_ref": "Install steps: run vle ingest...",
		"a2_ref": "Config keys: VLE_*",
		"b1_ref": "How to query the API.",
	}}

	s := retrieval.NewAgentic(llm, fetcher)

	res, err := s.SelectWithCost(context.Background(), tr, "how do I install?", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.SelectedIDs) != 1 || res.SelectedIDs[0] != "sec_a1" {
		t.Errorf("want [sec_a1], got %v", res.SelectedIDs)
	}
	if res.HopsTaken != 3 {
		t.Errorf("want HopsTaken=3, got %d", res.HopsTaken)
	}
	if res.Reasoning == "" {
		t.Errorf("want non-empty reasoning, got %q", res.Reasoning)
	}
	if res.Usage.LLMCalls != 3 {
		t.Errorf("want Usage.LLMCalls=3, got %d", res.Usage.LLMCalls)
	}

	// Sanity-check that the read action genuinely materialized the body
	// content into the next prompt — the read observation must contain
	// the body string the fetcher returned.
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.lastPrompts) < 3 {
		t.Fatalf("expected 3 prompts captured, got %d", len(llm.lastPrompts))
	}
	thirdPrompt := llm.lastPrompts[2]
	if !strings.Contains(thirdPrompt, "Install steps: run vle ingest") {
		t.Errorf("expected read observation to contain body content, got:\n%s", thirdPrompt)
	}
}

// TestAgenticExpandExpandDone covers the plan's manual trace: when the
// model picks expand → expand → done, the strategy must return the
// IDs from the final done action. This is the "two-level navigation
// without a read" scenario, which is the cheapest happy path.
func TestAgenticExpandExpandDone(t *testing.T) {
	t.Parallel()

	tr := buildAgenticTree()
	llm := &scriptedLLM{
		replies: []string{
			`{"action":"expand","section_id":"sec_a"}`,
			`{"action":"expand","section_id":"sec_b"}`,
			`{"action":"done","picked_ids":["sec_a1","sec_b1"]}`,
		},
	}
	s := retrieval.NewAgentic(llm, mapFetcher{data: map[string]string{}})

	res, err := s.SelectWithCost(context.Background(), tr, "anything", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.SelectedIDs) != 2 {
		t.Fatalf("want 2 ids, got %v", res.SelectedIDs)
	}
	want := map[tree.SectionID]bool{"sec_a1": true, "sec_b1": true}
	for _, id := range res.SelectedIDs {
		if !want[id] {
			t.Errorf("unexpected id %q", id)
		}
	}
	if res.HopsTaken != 3 {
		t.Errorf("want HopsTaken=3, got %d", res.HopsTaken)
	}
}

func TestAgenticHopCap(t *testing.T) {
	t.Parallel()

	tr := buildAgenticTree()
	// Never emits done. Strategy should bail at MaxHops.
	llm := &scriptedLLM{
		loopReply: `{"action":"expand","section_id":"sec_a"}`,
	}
	s := retrieval.NewAgentic(llm, mapFetcher{data: map[string]string{}})
	s.MaxHops = 4

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.HopsTaken != 4 {
		t.Errorf("want HopsTaken=4 (capped), got %d", res.HopsTaken)
	}
	// No done means no final picks.
	if len(res.SelectedIDs) != 0 {
		t.Errorf("want empty SelectedIDs on cap, got %v", res.SelectedIDs)
	}
	if got := atomic.LoadInt32(&llm.calls); got != 4 {
		t.Errorf("want 4 LLM calls (MaxHops), got %d", got)
	}
	if res.Usage.LLMCalls != 4 {
		t.Errorf("want Usage.LLMCalls=4, got %d", res.Usage.LLMCalls)
	}
}

// TestAgenticBadJSONGraceful covers the JSON-mode-blip path: a
// response that isn't valid JSON must not 500 the query. The first
// turn parses as garbage; we expect the strategy to nudge the model
// with a retry prompt and then bail when subsequent turns also fail.
// Final outcome: nil error, empty selection.
func TestAgenticBadJSONGraceful(t *testing.T) {
	t.Parallel()

	tr := buildAgenticTree()
	llm := &scriptedLLM{
		// Every reply is prose, never JSON. The loop must consume MaxHops
		// turns and return cleanly.
		loopReply: "I think it's sec_a1, that's where install lives.",
	}
	s := retrieval.NewAgentic(llm, mapFetcher{data: map[string]string{}})
	s.MaxHops = 3

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("want nil error on persistent parse failure, got %v", err)
	}
	if len(res.SelectedIDs) != 0 {
		t.Errorf("want empty selection on parse failure, got %v", res.SelectedIDs)
	}
	if res.HopsTaken != 3 {
		t.Errorf("want HopsTaken=3 (capped), got %d", res.HopsTaken)
	}
}

// TestAgenticFiltersUnknownPicks mirrors single-pass: if the model
// invents IDs not present in the tree, they must be dropped.
func TestAgenticFiltersUnknownPicks(t *testing.T) {
	t.Parallel()

	tr := buildAgenticTree()
	llm := &scriptedLLM{
		replies: []string{
			`{"action":"done","picked_ids":["sec_a1","sec_fake","sec_a1"]}`,
		},
	}
	s := retrieval.NewAgentic(llm, mapFetcher{data: map[string]string{}})

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SelectedIDs) != 1 || res.SelectedIDs[0] != "sec_a1" {
		t.Errorf("want [sec_a1] after filter+dedup, got %v", res.SelectedIDs)
	}
	if res.HopsTaken != 1 {
		t.Errorf("want HopsTaken=1, got %d", res.HopsTaken)
	}
}

// TestAgenticEmptyTree exercises the early-return guard so callers
// don't pay an LLM hop for a degenerate input.
func TestAgenticEmptyTree(t *testing.T) {
	t.Parallel()

	llm := &scriptedLLM{}
	s := retrieval.NewAgentic(llm, mapFetcher{data: map[string]string{}})

	res, err := s.SelectWithCost(context.Background(), &tree.Tree{}, "q", retrieval.ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SelectedIDs) != 0 {
		t.Errorf("want empty selection on empty tree, got %v", res.SelectedIDs)
	}
	if atomic.LoadInt32(&llm.calls) != 0 {
		t.Errorf("want 0 LLM calls on empty tree, got %d", llm.calls)
	}
}

// TestAgenticReadFallbackWhenNoContent verifies a 'read' on an
// internal section (no ContentRef) falls back to the summary rather
// than erroring out — the model should still get useful signal.
func TestAgenticReadFallbackWhenNoContent(t *testing.T) {
	t.Parallel()

	tr := buildAgenticTree()
	llm := &scriptedLLM{
		replies: []string{
			// Read on an internal section (sec_a has no ContentRef).
			`{"action":"read","section_id":"sec_a"}`,
			`{"action":"done","picked_ids":["sec_a1"]}`,
		},
	}
	s := retrieval.NewAgentic(llm, mapFetcher{data: map[string]string{}})

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SelectedIDs) != 1 || res.SelectedIDs[0] != "sec_a1" {
		t.Errorf("want [sec_a1], got %v", res.SelectedIDs)
	}

	// The observation for the read turn should contain "summary" — the
	// fallback path — not a fetcher error.
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.lastPrompts) < 2 {
		t.Fatalf("expected 2 prompts captured, got %d", len(llm.lastPrompts))
	}
	if !strings.Contains(llm.lastPrompts[1], "summary") {
		t.Errorf("expected summary fallback in read observation, got:\n%s", llm.lastPrompts[1])
	}
}

func TestParseAction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		in     string
		want   retrieval.Action
		hasErr bool
	}{
		{
			name: "expand",
			in:   `{"action":"expand","section_id":"sec_a"}`,
			want: retrieval.Action{Action: "expand", SectionID: "sec_a"},
		},
		{
			name: "outline_with_level",
			in:   `{"action":"outline","level":3}`,
			want: retrieval.Action{Action: "outline", Level: 3},
		},
		{
			name: "done_with_picks",
			in:   `{"action":"done","picked_ids":["a","b"],"reasoning":"why"}`,
			want: retrieval.Action{Action: "done", PickedIDs: []string{"a", "b"}, Reasoning: "why"},
		},
		{
			name: "code_fence",
			in:   "```json\n{\"action\":\"done\",\"picked_ids\":[\"x\"]}\n```",
			want: retrieval.Action{Action: "done", PickedIDs: []string{"x"}},
		},
		{
			name: "prose_before",
			in:   `Sure: {"action":"expand","section_id":"sec_a"}`,
			want: retrieval.Action{Action: "expand", SectionID: "sec_a"},
		},
		{
			name:   "garbage",
			in:     "I think it's sec_a",
			hasErr: true,
		},
		{
			name:   "empty",
			in:     "",
			hasErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := retrieval.ParseAction(c.in)
			if c.hasErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got.Action != c.want.Action {
				t.Errorf("Action: got %q want %q", got.Action, c.want.Action)
			}
			if got.SectionID != c.want.SectionID {
				t.Errorf("SectionID: got %q want %q", got.SectionID, c.want.SectionID)
			}
			if got.Level != c.want.Level {
				t.Errorf("Level: got %d want %d", got.Level, c.want.Level)
			}
			if len(got.PickedIDs) != len(c.want.PickedIDs) {
				t.Errorf("PickedIDs: got %v want %v", got.PickedIDs, c.want.PickedIDs)
			}
		})
	}
}
