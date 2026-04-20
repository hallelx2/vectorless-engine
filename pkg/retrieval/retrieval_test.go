package retrieval_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// mockLLM returns canned JSON responses based on which section IDs appear in
// the prompt — lets tests assert both single-pass and chunked-tree behavior
// without a real model.
type mockLLM struct {
	// pickIfPresent: if any of these IDs appears in the user prompt, pick it.
	pickIfPresent []tree.SectionID
	// reply overrides pickIfPresent when non-empty: returns exactly this JSON.
	reply string
	// calls counts how many times Complete has been invoked.
	calls int32
	// lastPrompts stores every user prompt seen (for assertions).
	mu          sync.Mutex
	lastPrompts []string
}

func (m *mockLLM) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	atomic.AddInt32(&m.calls, 1)

	// Grab the last user message.
	var userMsg string
	for _, msg := range req.Messages {
		if msg.Role == llmgate.RoleUser {
			userMsg = msg.Content
		}
	}
	m.mu.Lock()
	m.lastPrompts = append(m.lastPrompts, userMsg)
	m.mu.Unlock()

	if m.reply != "" {
		return &llmgate.Response{Content: m.reply}, nil
	}

	var picks []string
	for _, id := range m.pickIfPresent {
		if strings.Contains(userMsg, string(id)) {
			picks = append(picks, string(id))
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"selected_section_ids": picks,
		"reasoning":            "mock",
	})
	return &llmgate.Response{Content: string(raw)}, nil
}

func (m *mockLLM) CountTokens(ctx context.Context, s string) (int, error) {
	// 1 token per 4 chars, matching Anthropic's fallback heuristic.
	return len(s) / 4, nil
}

// buildTree constructs a tiny test tree: root "Atlas" with 3 leaf children.
func buildTree() *tree.Tree {
	root := &tree.Section{
		ID: "sec_root", Title: "Atlas",
		Children: []*tree.Section{
			{ID: "sec_a", ParentID: "sec_root", Title: "Setup", Summary: "install steps"},
			{ID: "sec_b", ParentID: "sec_root", Title: "Usage", Summary: "how to query"},
			{ID: "sec_c", ParentID: "sec_root", Title: "FAQ", Summary: "common questions"},
		},
	}
	return &tree.Tree{DocumentID: "doc_x", Title: "Atlas", Root: root}
}

func TestSinglePassHappy(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_b"}}
	s := retrieval.NewSinglePass(m)

	ids, err := s.Select(context.Background(), tr, "how do I query?", retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(ids) != 1 || ids[0] != "sec_b" {
		t.Errorf("want [sec_b], got %v", ids)
	}
	if atomic.LoadInt32(&m.calls) != 1 {
		t.Errorf("want 1 call, got %d", m.calls)
	}
}

func TestSinglePassFiltersUnknownIDs(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{reply: `{"selected_section_ids":["sec_a","sec_fake","sec_a"],"reasoning":"x"}`}
	s := retrieval.NewSinglePass(m)

	ids, err := s.Select(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "sec_a" {
		t.Errorf("want [sec_a] (dedup+filter), got %v", ids)
	}
}

func TestSinglePassToleratesCodeFences(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{reply: "```json\n{\"selected_section_ids\":[\"sec_c\"]}\n```"}
	s := retrieval.NewSinglePass(m)

	ids, err := s.Select(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "sec_c" {
		t.Errorf("want [sec_c], got %v", ids)
	}
}

func TestParseSelection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []tree.SectionID
	}{
		{"plain", `{"selected_section_ids":["a","b"]}`, []tree.SectionID{"a", "b"}},
		{"empty", `{"selected_section_ids":[]}`, nil},
		{"prose_before", `Sure, here you go: {"selected_section_ids":["x"]}`, []tree.SectionID{"x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := retrieval.ParseSelection(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("len: got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("idx %d: got %q want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestChunkedTreeSinglesliceWhenItFits(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a", "sec_b"}}
	s := retrieval.NewChunkedTree(m)

	ids, err := s.Select(context.Background(), tr, "q", retrieval.ContextBudget{
		MaxTokens: 100000, MaxParallelCalls: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 ids, got %v", ids)
	}
	// UnionMerge returns sorted output.
	if ids[0] != "sec_a" || ids[1] != "sec_b" {
		t.Errorf("want sorted [sec_a sec_b], got %v", ids)
	}
	// Small tree → single slice → single call.
	if c := atomic.LoadInt32(&m.calls); c != 1 {
		t.Errorf("want 1 llm call for fit-in-one-slice, got %d", c)
	}
}

func TestChunkedTreeSplitsWhenOverBudget(t *testing.T) {
	// Give a very small budget so each child must live in its own slice.
	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a", "sec_c"}}
	s := retrieval.NewChunkedTree(m)

	budget := retrieval.ContextBudget{
		// Each rendered section line is ~50 chars ≈ 12 tokens. 25-token budget
		// forces the splitter to make one slice per child.
		MaxTokens:         25,
		ReservedForPrompt: 0,
		MaxParallelCalls:  4,
	}
	ids, err := s.Select(context.Background(), tr, "q", budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "sec_a" || ids[1] != "sec_c" {
		t.Errorf("want [sec_a sec_c], got %v", ids)
	}
	if c := atomic.LoadInt32(&m.calls); c < 2 {
		t.Errorf("want >=2 llm calls (one per slice), got %d", c)
	}
}

func TestChunkedTreeIDFabricationIsFiltered(t *testing.T) {
	tr := buildTree()
	// Model tries to pick IDs not in any slice.
	m := &mockLLM{reply: `{"selected_section_ids":["sec_a","sec_made_up"]}`}
	s := retrieval.NewChunkedTree(m)

	ids, err := s.Select(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == "sec_made_up" {
			t.Fatalf("fabricated id leaked through: %v", ids)
		}
	}
}

func TestDefaultSplitterFastPath(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{}
	tok := retrieval.LLMTokenizer{C: m}

	slices, err := retrieval.NewDefaultSplitter().Split(context.Background(), tr,
		retrieval.ContextBudget{MaxTokens: 100000}, tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(slices) != 1 {
		t.Fatalf("small tree should fit in one slice, got %d", len(slices))
	}
	if !strings.Contains(slices[0].Breadcrumb, "Atlas") {
		t.Errorf("breadcrumb missing doc title: %q", slices[0].Breadcrumb)
	}
}
