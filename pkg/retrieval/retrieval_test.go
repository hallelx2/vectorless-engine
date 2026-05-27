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

// buildTreeWithCandidates returns a tree where sec_b carries HyDE
// candidate questions. Used to assert the retrieval prompt surfaces them.
func buildTreeWithCandidates() *tree.Tree {
	root := &tree.Section{
		ID: "sec_root", Title: "Atlas",
		Children: []*tree.Section{
			{ID: "sec_a", ParentID: "sec_root", Title: "Setup", Summary: "install steps"},
			{
				ID: "sec_b", ParentID: "sec_root", Title: "Usage", Summary: "how to query",
				CandidateQuestions: []string{
					"How do I run a query against the engine?",
					"What ports does the server use?",
				},
			},
			{ID: "sec_c", ParentID: "sec_root", Title: "FAQ", Summary: "common questions"},
		},
		PageStart: 1,
		PageEnd:   4,
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

// When the model returns prose without any JSON (Gemini's occasional JSON-mode
// blip), the strategy must retry and then degrade gracefully — empty selection
// with no error — instead of bubbling the parse failure up as a 500.
func TestSinglePassGracefulOnNonJSON(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{reply: "The most relevant section is the one about debt securities."}
	s := retrieval.NewSinglePass(m)

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatalf("want graceful nil error on persistent parse failure, got %v", err)
	}
	if len(res.SelectedIDs) != 0 {
		t.Errorf("want empty selection on parse failure, got %v", res.SelectedIDs)
	}
	// 1 initial attempt + 2 retries = 3 LLM calls, all counted in usage.
	if got := atomic.LoadInt32(&m.calls); got != 3 {
		t.Errorf("expected 3 LLM attempts (1 + 2 retries), got %d", got)
	}
	if res.Usage.LLMCalls != 3 {
		t.Errorf("expected Usage.LLMCalls=3, got %d", res.Usage.LLMCalls)
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
			got, confidences, err := retrieval.ParseSelection(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if confidences != nil {
				t.Errorf("legacy-shape input must not populate confidences, got %v", confidences)
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

// TestParseSelectionNewShape exercises the Phase 2.4 picks shape:
// each pick carries an id + confidence, the parser returns both the
// id list and a confidence map.
func TestParseSelectionNewShape(t *testing.T) {
	raw := `{"picks":[{"id":"sec_a","confidence":0.82},{"id":"sec_b","confidence":0.31}],"reasoning":"x"}`
	ids, confidences, err := retrieval.ParseSelection(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ids) != 2 || ids[0] != "sec_a" || ids[1] != "sec_b" {
		t.Fatalf("ids: got %v want [sec_a sec_b]", ids)
	}
	if confidences == nil {
		t.Fatal("confidences must be populated for new-shape response")
	}
	if got := confidences["sec_a"]; got != 0.82 {
		t.Errorf("sec_a confidence = %v, want 0.82", got)
	}
	if got := confidences["sec_b"]; got != 0.31 {
		t.Errorf("sec_b confidence = %v, want 0.31", got)
	}
}

// TestParseSelectionMixedShape covers a partially-populated new-shape
// response: some picks have confidence, others don't. The confidence
// map only surfaces IDs whose confidence was actually present —
// missing entries are NOT defaulted to 0 (which would force
// abstention) or to 1 (which would suppress it).
func TestParseSelectionMixedShape(t *testing.T) {
	raw := `{"picks":[{"id":"sec_a","confidence":0.9},{"id":"sec_b"},{"id":"sec_c","confidence":0.4}]}`
	ids, confidences, err := retrieval.ParseSelection(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("ids: got %v, want 3 picks", ids)
	}
	if _, present := confidences["sec_a"]; !present {
		t.Error("sec_a should have confidence")
	}
	if _, present := confidences["sec_b"]; present {
		t.Error("sec_b should NOT have confidence (model omitted it)")
	}
	if _, present := confidences["sec_c"]; !present {
		t.Error("sec_c should have confidence")
	}
	if confidences["sec_a"] != 0.9 || confidences["sec_c"] != 0.4 {
		t.Errorf("confidences = %v", confidences)
	}
}

// TestParseSelectionClampsConfidence asserts confidences outside
// [0.0, 1.0] are clamped — defence-in-depth against a model that
// returns 1.5 or -0.2 despite the prompt's range.
func TestParseSelectionClampsConfidence(t *testing.T) {
	raw := `{"picks":[{"id":"sec_a","confidence":1.7},{"id":"sec_b","confidence":-0.3}]}`
	_, confidences, err := retrieval.ParseSelection(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if confidences["sec_a"] != 1.0 {
		t.Errorf("sec_a clamped: want 1.0, got %v", confidences["sec_a"])
	}
	if confidences["sec_b"] != 0.0 {
		t.Errorf("sec_b clamped: want 0.0, got %v", confidences["sec_b"])
	}
}

// TestParseSelectionPicksDedup ensures duplicate IDs in `picks` are
// deduplicated (first-seen wins) so the strategy doesn't double-count
// a section the model accidentally listed twice.
func TestParseSelectionPicksDedup(t *testing.T) {
	raw := `{"picks":[{"id":"sec_a","confidence":0.7},{"id":"sec_a","confidence":0.2}]}`
	ids, confidences, err := retrieval.ParseSelection(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ids) != 1 || ids[0] != "sec_a" {
		t.Fatalf("ids: got %v want [sec_a]", ids)
	}
	if confidences["sec_a"] != 0.7 {
		t.Errorf("first-seen confidence should win: got %v want 0.7", confidences["sec_a"])
	}
}

// TestParseSelectionNewShapeNoConfidences covers a new-shape response
// where the model returned `picks` but stamped no confidence values
// at all — must be treated as legacy (nil confidences) so the API
// layer does NOT abstain on a confidence signal that isn't there.
func TestParseSelectionNewShapeNoConfidences(t *testing.T) {
	raw := `{"picks":[{"id":"sec_a"},{"id":"sec_b"}]}`
	ids, confidences, err := retrieval.ParseSelection(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids: got %v, want 2", ids)
	}
	if confidences != nil {
		t.Errorf("missing confidences must surface as nil map, got %v", confidences)
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

// TestSelectionPromptSurfacesCandidateQuestion asserts the rendered
// outline includes an "answers: ..." line per section that carries
// HyDE candidate questions. Only the first question is surfaced (to
// keep the prompt budget small) — this guards the contract retrieval
// depends on.
func TestSelectionPromptSurfacesCandidateQuestion(t *testing.T) {
	tr := buildTreeWithCandidates()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_b"}}
	s := retrieval.NewSinglePass(m)

	_, err := s.Select(context.Background(), tr, "querying", retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if atomic.LoadInt32(&m.calls) != 1 {
		t.Fatalf("want 1 call, got %d", m.calls)
	}
	m.mu.Lock()
	prompts := append([]string(nil), m.lastPrompts...)
	m.mu.Unlock()
	if len(prompts) == 0 {
		t.Fatal("no prompts captured")
	}
	prompt := prompts[0]
	if !strings.Contains(prompt, "answers: ") {
		t.Errorf("prompt missing answers hint:\n%s", prompt)
	}
	if !strings.Contains(prompt, "How do I run a query against the engine?") {
		t.Errorf("prompt missing first candidate question:\n%s", prompt)
	}
	// Only the FIRST question is surfaced — the second must NOT appear.
	if strings.Contains(prompt, "What ports does the server use?") {
		t.Errorf("prompt should surface only first candidate question, got both:\n%s", prompt)
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

// TestSinglePassReturnsConfidences asserts that a new-shape LLM
// response with confidence scores surfaces a populated Confidences
// map on the strategy's Result. The strategy itself never abstains —
// even when every confidence is below the typical 0.4 threshold the
// IDs still come back and the API layer decides what to do.
func TestSinglePassReturnsConfidences(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{reply: `{"picks":[{"id":"sec_a","confidence":0.78},{"id":"sec_b","confidence":0.12}],"reasoning":"x"}`}
	s := retrieval.NewSinglePass(m)

	res, err := s.SelectWithCost(context.Background(), tr, "q",
		retrieval.ContextBudget{ModelName: "model", MaxTokens: 1000})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.SelectedIDs) != 2 {
		t.Fatalf("want 2 IDs, got %v", res.SelectedIDs)
	}
	if res.Confidences == nil {
		t.Fatal("Confidences should be populated for new-shape response")
	}
	if got := res.Confidences["sec_a"]; got != 0.78 {
		t.Errorf("sec_a confidence = %v, want 0.78", got)
	}
	if got := res.Confidences["sec_b"]; got != 0.12 {
		t.Errorf("sec_b confidence = %v, want 0.12", got)
	}
}

// TestSinglePassAllLowConfidencesStillReturnsIDs is the abstention
// smoke contract from the spec: the strategy itself never abstains.
// Even when every confidence is below 0.4 the IDs come back. The
// API layer is the only place that may convert "all low" into an
// abstention.
func TestSinglePassAllLowConfidencesStillReturnsIDs(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{reply: `{"picks":[{"id":"sec_a","confidence":0.1},{"id":"sec_b","confidence":0.2}]}`}
	s := retrieval.NewSinglePass(m)

	res, err := s.SelectWithCost(context.Background(), tr, "q",
		retrieval.ContextBudget{ModelName: "model", MaxTokens: 1000})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.SelectedIDs) != 2 {
		t.Fatalf("strategy must return IDs even with low confidences, got %v", res.SelectedIDs)
	}
	if len(res.Confidences) != 2 {
		t.Errorf("Confidences should mirror SelectedIDs, got %v", res.Confidences)
	}
}

// TestSinglePassLegacyShapeNoConfidences confirms that the legacy
// response shape continues to work after the new-shape refactor.
// Critically, Confidences stays nil so the API layer does not abstain.
func TestSinglePassLegacyShapeNoConfidences(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{reply: `{"selected_section_ids":["sec_a","sec_b"],"reasoning":"x"}`}
	s := retrieval.NewSinglePass(m)

	res, err := s.SelectWithCost(context.Background(), tr, "q",
		retrieval.ContextBudget{ModelName: "model", MaxTokens: 1000})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.SelectedIDs) != 2 {
		t.Fatalf("legacy response shape must still work, got %v", res.SelectedIDs)
	}
	if res.Confidences != nil {
		t.Errorf("legacy response must NOT populate Confidences, got %v", res.Confidences)
	}
}

// TestChunkedTreeMergesConfidences verifies the chunked-tree strategy
// surfaces confidences in the merged Result. Because the test tree
// is small enough to fit in one slice, this is effectively a single
// slice union — but the field still has to round-trip through the
// per-slice plumbing.
func TestChunkedTreeMergesConfidences(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{reply: `{"picks":[{"id":"sec_a","confidence":0.6},{"id":"sec_c","confidence":0.9}]}`}
	s := retrieval.NewChunkedTree(m)

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{
		ModelName: "model", MaxTokens: 100000, MaxParallelCalls: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Confidences) != 2 {
		t.Fatalf("Confidences should carry both picks, got %v", res.Confidences)
	}
	if res.Confidences["sec_a"] != 0.6 || res.Confidences["sec_c"] != 0.9 {
		t.Errorf("confidences = %v", res.Confidences)
	}
}

// TestSinglePassStampsTraceToken verifies that SelectWithCost
// populates a 64-char hex TraceToken on the returned Result.
func TestSinglePassStampsTraceToken(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_b"}}
	s := retrieval.NewSinglePass(m)

	res, err := s.SelectWithCost(context.Background(), tr, "q",
		retrieval.ContextBudget{ModelName: "claude-sonnet-4-5", MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TraceToken) != 64 {
		t.Fatalf("trace_token must be 64 chars, got %d (%q)", len(res.TraceToken), res.TraceToken)
	}
	for _, r := range res.TraceToken {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("trace_token must be lowercase hex, got %q", res.TraceToken)
		}
	}

	// Same inputs → same token.
	res2, err := s.SelectWithCost(context.Background(), tr, "q",
		retrieval.ContextBudget{ModelName: "claude-sonnet-4-5", MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if res2.TraceToken != res.TraceToken {
		t.Errorf("same inputs must produce same trace_token: %q vs %q",
			res.TraceToken, res2.TraceToken)
	}
}

// TestChunkedTreeStampsTraceToken verifies that the chunked-tree
// strategy populates the trace token on its returned Result.
func TestChunkedTreeStampsTraceToken(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a", "sec_b"}}
	s := retrieval.NewChunkedTree(m)

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{
		ModelName: "claude-sonnet-4-5", MaxTokens: 100000, MaxParallelCalls: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TraceToken) != 64 {
		t.Fatalf("chunked-tree trace_token must be 64 chars, got %d", len(res.TraceToken))
	}
}

// buildTreeWithAxes returns a tree where sec_a carries a multi-axis
// summary. Used to assert the retrieval prompt surfaces entities +
// numbers on the section line.
func buildTreeWithAxes() *tree.Tree {
	root := &tree.Section{
		ID: "sec_root", Title: "Atlas",
		Children: []*tree.Section{
			{
				ID: "sec_a", ParentID: "sec_root", Title: "Long-Term Debt",
				Summary: "issued debt securities, repayment schedules",
				SummaryAxes: &tree.SummaryAxes{
					Topics:   []string{"debt", "long-term-obligations"},
					Entities: []string{"3M Company", "JPMorgan", "BofA", "Wells Fargo"},
					Numbers:  []string{"$4.2B", "2034", "2.8%", "2027"},
					OneLine:  "issued debt securities, repayment schedules",
				},
			},
			{
				ID: "sec_b", ParentID: "sec_root", Title: "Revenue",
				Summary: "fiscal-year-over-year revenue",
				// Empty axes pointer — axes block exists but has no
				// entities/numbers. Tests the non-rendering branch.
				SummaryAxes: &tree.SummaryAxes{OneLine: "fiscal-year-over-year revenue"},
			},
			{ID: "sec_c", ParentID: "sec_root", Title: "FAQ", Summary: "common questions"},
		},
	}
	return &tree.Tree{DocumentID: "doc_x", Title: "Atlas", Root: root}
}

// TestSelectionPromptSurfacesAxes is the Phase 2.5 retrieval-prompt
// contract: when a section carries a SummaryAxes, the outline line
// must render entities and numbers (truncated to first 3 each) so the
// retrieval model has direct surface-form access to proper-noun and
// numeric anchors.
func TestSelectionPromptSurfacesAxes(t *testing.T) {
	tr := buildTreeWithAxes()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a"}}
	s := retrieval.NewSinglePass(m)

	_, err := s.Select(context.Background(), tr, "how much debt?",
		retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	m.mu.Lock()
	prompts := append([]string(nil), m.lastPrompts...)
	m.mu.Unlock()
	if len(prompts) == 0 {
		t.Fatal("no prompts captured")
	}
	prompt := prompts[0]

	// Entities + numbers appear with their "— entities: " / "— numbers: "
	// prefixes on sec_a's line.
	if !strings.Contains(prompt, "entities: ") {
		t.Errorf("prompt missing entities prefix:\n%s", prompt)
	}
	if !strings.Contains(prompt, "numbers: ") {
		t.Errorf("prompt missing numbers prefix:\n%s", prompt)
	}
	if !strings.Contains(prompt, "3M Company") {
		t.Errorf("prompt missing first entity:\n%s", prompt)
	}
	if !strings.Contains(prompt, "$4.2B") {
		t.Errorf("prompt missing first number:\n%s", prompt)
	}
	// Truncation: only first 3 entities / numbers are rendered. The
	// 4th of each must NOT appear.
	if strings.Contains(prompt, "Wells Fargo") {
		t.Errorf("entities should be truncated to first 3, 4th leaked:\n%s", prompt)
	}
	if strings.Contains(prompt, "2027") {
		t.Errorf("numbers should be truncated to first 3, 4th leaked:\n%s", prompt)
	}

	// sec_b has an axes object but empty lists — entities/numbers
	// labels must NOT appear on sec_b's line. We assert by checking
	// that "Revenue — entities" does not appear (Revenue is sec_b's
	// title).
	if strings.Contains(prompt, "Revenue — entities") || strings.Contains(prompt, "Revenue — numbers") {
		t.Errorf("sec_b has empty axis lists; should not render entities/numbers:\n%s", prompt)
	}

	// sec_c has no axes at all (nil pointer) — the rendering must
	// skip cleanly. Same check on sec_c's title.
	if strings.Contains(prompt, "FAQ — entities") || strings.Contains(prompt, "FAQ — numbers") {
		t.Errorf("sec_c has no axes; should not render axes block:\n%s", prompt)
	}
}

// TestSelectionPromptOmitsAxesForOldSections guards the
// backwards-compatibility contract: sections written before Phase 2.5
// have axes==nil, and the retrieval prompt must continue to render
// exactly as it did before this PR. The pre-axes prompt shape is
// `- [id] Title — summary` with no further suffixes; we assert by
// making sure the entities/numbers labels are absent.
func TestSelectionPromptOmitsAxesForOldSections(t *testing.T) {
	tr := buildTree() // pre-Phase-2.5 tree, no SummaryAxes anywhere
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_b"}}
	s := retrieval.NewSinglePass(m)

	_, err := s.Select(context.Background(), tr, "q",
		retrieval.ContextBudget{MaxTokens: 1000})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	m.mu.Lock()
	prompts := append([]string(nil), m.lastPrompts...)
	m.mu.Unlock()
	if len(prompts) == 0 {
		t.Fatal("no prompts captured")
	}
	prompt := prompts[0]
	if strings.Contains(prompt, "entities: ") || strings.Contains(prompt, "numbers: ") {
		t.Errorf("axes labels must not appear when no section has axes:\n%s", prompt)
	}
}

// TestTraceTokenMatchesExternalComputation ties the strategy output to
// the canonical ComputeTraceToken helper, so any drift between the
// helper and the per-strategy plumbing is caught at test time.
func TestTraceTokenMatchesExternalComputation(t *testing.T) {
	tr := buildTree()
	m := &mockLLM{pickIfPresent: []tree.SectionID{"sec_a"}}
	s := retrieval.NewSinglePass(m)
	model := "claude-sonnet-4-5"

	res, err := s.SelectWithCost(context.Background(), tr, "q",
		retrieval.ContextBudget{ModelName: model, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	want := retrieval.ComputeTraceToken(tr.DocumentID, "1", model, res.SelectedIDs)
	if res.TraceToken != want {
		t.Errorf("strategy trace_token %q does not match ComputeTraceToken %q",
			res.TraceToken, want)
	}
}
