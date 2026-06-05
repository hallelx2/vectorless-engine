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

// pageScriptedLLM is a scriptedLLM for the TreeWalk strategy.
// Each Complete call returns the next canned response. When the
// script is exhausted, loopReply (if set) is returned on every
// subsequent call — the hop-cap test uses this to simulate a model
// that never emits done.
type pageScriptedLLM struct {
	replies   []string
	loopReply string

	calls int32

	mu          sync.Mutex
	lastPrompts []string
}

func (p *pageScriptedLLM) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	i := int(atomic.AddInt32(&p.calls, 1)) - 1

	var userMsg string
	for _, msg := range req.Messages {
		if msg.Role == llmgate.RoleUser {
			userMsg = msg.Content
		}
	}
	p.mu.Lock()
	p.lastPrompts = append(p.lastPrompts, userMsg)
	p.mu.Unlock()

	if i < len(p.replies) {
		return &llmgate.Response{Content: p.replies[i]}, nil
	}
	if p.loopReply != "" {
		return &llmgate.Response{Content: p.loopReply}, nil
	}
	return nil, errors.New("pageScriptedLLM: replies exhausted")
}

func (p *pageScriptedLLM) CountTokens(ctx context.Context, t string) (int, error) {
	return len(t) / 4, nil
}

// pageMapLoader is an in-memory PageContentLoader backed by a map.
type pageMapLoader struct{ data map[string]string }

func (m pageMapLoader) Load(ctx context.Context, ref string) ([]byte, error) {
	v, ok := m.data[ref]
	if !ok {
		return nil, errors.New("not found")
	}
	return []byte(v), nil
}

// pageStaticTOC is a TOCProvider that returns a canned JSON blob.
// Tests use this to assert the get_document_structure observation
// surfaces the persisted TOC ahead of the synthesised fallback.
type pageStaticTOC struct{ blob []byte }

func (p pageStaticTOC) GetTOC(ctx context.Context, _ tree.DocumentID) ([]byte, error) {
	return p.blob, nil
}

// pageErroringTOC simulates documents.toc_tree being NULL (no
// LLM-built TOC yet). The strategy must degrade to the synthesised
// view rather than failing the request.
type pageErroringTOC struct{}

func (pageErroringTOC) GetTOC(ctx context.Context, _ tree.DocumentID) ([]byte, error) {
	return nil, retrieval.ErrNoTOC
}

// buildPagedTree mirrors buildAgenticTree but stamps page_start /
// page_end on every section so TreeWalkStrategy can navigate. The
// shape:
//
//	sec_root → [sec_a (1-4), sec_b (5-9)]
//	  sec_a → [sec_a1 (1-2 install), sec_a2 (3-4 config)]
//	  sec_b → [sec_b1 (5-7 querying), sec_b2 (8-9 debt)]
func buildPagedTree() *tree.Tree {
	a1 := &tree.Section{ID: "sec_a1", ParentID: "sec_a", Title: "Install", Summary: "install steps", ContentRef: "a1_ref", PageStart: 1, PageEnd: 2}
	a2 := &tree.Section{ID: "sec_a2", ParentID: "sec_a", Title: "Config", Summary: "config keys", ContentRef: "a2_ref", PageStart: 3, PageEnd: 4}
	b1 := &tree.Section{ID: "sec_b1", ParentID: "sec_b", Title: "Querying", Summary: "how to query", ContentRef: "b1_ref", PageStart: 5, PageEnd: 7}
	b2 := &tree.Section{ID: "sec_b2", ParentID: "sec_b", Title: "Debt", Summary: "long-term debt", ContentRef: "b2_ref", PageStart: 8, PageEnd: 9}
	a := &tree.Section{ID: "sec_a", ParentID: "sec_root", Title: "Setup", Summary: "setup section", Children: []*tree.Section{a1, a2}, PageStart: 1, PageEnd: 4}
	b := &tree.Section{ID: "sec_b", ParentID: "sec_root", Title: "Usage", Summary: "usage section", Children: []*tree.Section{b1, b2}, PageStart: 5, PageEnd: 9}
	root := &tree.Section{ID: "sec_root", Title: "Atlas", Children: []*tree.Section{a, b}, PageStart: 1, PageEnd: 9}
	return &tree.Tree{DocumentID: "doc_x", Title: "Atlas", Root: root}
}

// TestTreeWalkHappyPath drives the canonical 3-tool sequence:
// structure → get_pages → done. We assert the strategy:
//   - returns the answer string in Result.Reasoning
//   - lists the section IDs whose page range overlaps the citation
//   - records the get_pages call in PagesRead
//   - tracks HopsTaken correctly
//   - computes a non-empty TraceToken keyed by the cited pages
func TestTreeWalkHappyPath(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_document_structure","reasoning":"orient"}`,
			`{"tool":"get_pages","start_page":1,"end_page":2,"reasoning":"install lives near the front"}`,
			`{"tool":"done","answer":"Run vle ingest then start the server.","cited_pages":[[1,2]],"reasoning":"install steps live on pages 1-2"}`,
		},
	}
	loader := pageMapLoader{data: map[string]string{
		"a1_ref": "Install steps: run vle ingest...",
		"a2_ref": "Config keys: VLE_*",
		"b1_ref": "How to query the API.",
		"b2_ref": "Debt registration is in line items A and B.",
	}}

	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = loader

	res, err := s.SelectWithCost(context.Background(), tr, "how do I install?", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.HopsTaken != 3 {
		t.Errorf("HopsTaken = %d, want 3", res.HopsTaken)
	}
	if res.Usage.LLMCalls != 3 {
		t.Errorf("Usage.LLMCalls = %d, want 3", res.Usage.LLMCalls)
	}
	if !strings.Contains(res.Reasoning, "vle ingest") {
		t.Errorf("Reasoning (answer) must contain the model's reply, got %q", res.Reasoning)
	}
	if len(res.SelectedIDs) == 0 {
		t.Fatalf("SelectedIDs must include sections covering pages 1-2, got %v", res.SelectedIDs)
	}
	// sec_a1 (1-2) is the leaf — must be in the list. sec_a (1-4)
	// and the synthetic sec_root (1-9) overlap too because page
	// ranges intersect. The strategy's job is to surface ANY section
	// whose [page_start,page_end] overlaps the citation; the API
	// layer narrows further if it cares.
	wantIDs := map[tree.SectionID]bool{"sec_a1": true, "sec_a": true, "sec_root": true}
	for _, id := range res.SelectedIDs {
		if !wantIDs[id] {
			t.Errorf("unexpected section ID %q (only sections overlapping pages 1-2 may appear)", id)
		}
	}
	if _, ok := indexOfSection(res.SelectedIDs, "sec_a1"); !ok {
		t.Errorf("sec_a1 must be in SelectedIDs, got %v", res.SelectedIDs)
	}
	if len(res.PagesRead) != 1 {
		t.Fatalf("PagesRead = %v, want 1 entry", res.PagesRead)
	}
	if res.PagesRead[0].StartPage != 1 || res.PagesRead[0].EndPage != 2 {
		t.Errorf("PagesRead[0] = %+v, want 1-2", res.PagesRead[0])
	}
	if res.PagesRead[0].CharCount == 0 {
		t.Errorf("PagesRead[0].CharCount must be non-zero, got %d", res.PagesRead[0].CharCount)
	}
	if res.TraceToken == "" {
		t.Errorf("TraceToken must be populated on success")
	}

	// Assert the second prompt — the one that follows the
	// get_document_structure call — actually surfaced the
	// synthesised TOC (since no TOC provider was wired). It must
	// contain section titles.
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.lastPrompts) < 2 {
		t.Fatalf("expected at least 2 prompts captured, got %d", len(llm.lastPrompts))
	}
	if !strings.Contains(llm.lastPrompts[1], "Install") {
		t.Errorf("get_document_structure observation should include section titles; got:\n%s", llm.lastPrompts[1])
	}
	if !strings.Contains(llm.lastPrompts[2], "Install steps: run vle ingest") {
		t.Errorf("get_pages observation should include loaded content; got:\n%s", llm.lastPrompts[2])
	}
}

// buildMinimalIngestedTree mirrors the post-ingest shape of a document
// run through MINIMAL ingest mode: sections carry page ranges (the PDF
// parser populates them) and content refs (persisted bodies) but NO
// summaries (minimal mode skips the summarize stage) and NO HyDE
// questions. documents.toc_tree is NULL after minimal ingest, which the
// strategy models by leaving TOC nil — forcing synthesiseTOC.
func buildMinimalIngestedTree() *tree.Tree {
	a1 := &tree.Section{ID: "sec_a1", ParentID: "sec_a", Title: "Ownership", ContentRef: "a1_ref", PageStart: 1, PageEnd: 2}
	a2 := &tree.Section{ID: "sec_a2", ParentID: "sec_a", Title: "Borrowing", ContentRef: "a2_ref", PageStart: 3, PageEnd: 4}
	b1 := &tree.Section{ID: "sec_b1", ParentID: "sec_b", Title: "Lifetimes", ContentRef: "b1_ref", PageStart: 5, PageEnd: 7}
	a := &tree.Section{ID: "sec_a", ParentID: "sec_root", Title: "Memory", Children: []*tree.Section{a1, a2}, PageStart: 1, PageEnd: 4}
	b := &tree.Section{ID: "sec_b", ParentID: "sec_root", Title: "Advanced", Children: []*tree.Section{b1}, PageStart: 5, PageEnd: 7}
	root := &tree.Section{ID: "sec_root", Title: "Rust", Children: []*tree.Section{a, b}, PageStart: 1, PageEnd: 7}
	return &tree.Tree{DocumentID: "doc_minimal", Title: "Rust", Root: root}
}

// TestTreeWalkMinimalIngestedDoc is the cross-package guarantee for the
// minimal ingest mode: a document ingested with NO LLM enrichment (no
// summaries, no HyDE, NULL toc_tree) is still fully answerable through
// the page-based strategy. It drives the canonical structure → get_pages
// → done loop with TOC left nil (the NULL-toc_tree state) and asserts:
//
//   - get_document_structure surfaces the SYNTHESISED TOC (section titles
//     from the tree) — proving the NULL-toc_tree fallback works; and
//   - get_pages surfaces RAW section content read via the loader — the
//     text the strategy answers from, which on a real minimal-ingested
//     doc is the persisted page text (and still contains any table text).
//
// No summaries are present anywhere in the tree, so this also proves the
// strategy does not hard-require a summary to navigate or answer.
func TestTreeWalkMinimalIngestedDoc(t *testing.T) {
	t.Parallel()

	tr := buildMinimalIngestedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_document_structure","reasoning":"orient by titles"}`,
			`{"tool":"get_pages","start_page":1,"end_page":2,"reasoning":"ownership lives up front"}`,
			`{"tool":"done","answer":"Ownership is a set of rules the compiler checks.","cited_pages":[[1,2]],"reasoning":"pages 1-2 define ownership"}`,
		},
	}
	loader := pageMapLoader{data: map[string]string{
		"a1_ref": "Ownership is a set of rules that govern how a Rust program manages memory.",
		"a2_ref": "References borrow a value without taking ownership.",
		"b1_ref": "Lifetimes ensure references are valid.",
	}}

	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = loader
	// s.TOC intentionally left nil — models the NULL documents.toc_tree
	// state minimal ingest leaves behind. The strategy must synthesise.

	res, err := s.SelectWithCost(context.Background(), tr, "what is ownership?", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost on minimal-ingested doc: %v", err)
	}
	if !strings.Contains(res.Reasoning, "Ownership is a set of rules") {
		t.Errorf("answer must carry the model's reply, got %q", res.Reasoning)
	}
	if _, ok := indexOfSection(res.SelectedIDs, "sec_a1"); !ok {
		t.Errorf("sec_a1 (pages 1-2) must be cited, got %v", res.SelectedIDs)
	}
	if len(res.PagesRead) != 1 || res.PagesRead[0].CharCount == 0 {
		t.Errorf("expected one non-empty get_pages read, got %+v", res.PagesRead)
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.lastPrompts) < 3 {
		t.Fatalf("expected >=3 prompts captured, got %d", len(llm.lastPrompts))
	}
	// (1) Synthesised TOC carried a section title (no toc_tree provider).
	if !strings.Contains(llm.lastPrompts[1], "Ownership") {
		t.Errorf("synthesised TOC observation should include section titles; got:\n%s", llm.lastPrompts[1])
	}
	// (2) get_pages carried the RAW persisted body, not a summary.
	if !strings.Contains(llm.lastPrompts[2], "Ownership is a set of rules that govern") {
		t.Errorf("get_pages observation should include raw section content; got:\n%s", llm.lastPrompts[2])
	}
}

// TestTreeWalkMultiRangeDone covers a done with two cited ranges:
// the strategy must surface every section that overlaps EITHER
// range. This is the FinanceBench-shaped pattern: an answer that
// pulls evidence from two unrelated parts of a 10-K.
func TestTreeWalkMultiRangeDone(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_document_structure"}`,
			`{"tool":"get_pages","start_page":3,"end_page":4}`,
			`{"tool":"get_pages","start_page":8,"end_page":9}`,
			`{"tool":"done","answer":"Config is X. Debt is Y.","cited_pages":[[3,4],[8,9]]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{
		"a2_ref": "Config keys: VLE_*",
		"b2_ref": "Debt registration is in line items A and B.",
	}}

	res, err := s.SelectWithCost(context.Background(), tr, "config and debt?", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.HopsTaken != 4 {
		t.Errorf("HopsTaken = %d, want 4", res.HopsTaken)
	}
	if len(res.PagesRead) != 2 {
		t.Fatalf("PagesRead = %v, want 2 entries", res.PagesRead)
	}
	wantSecs := map[tree.SectionID]bool{
		"sec_a2": true, "sec_b2": true, // direct leaf overlaps
		"sec_a": true, "sec_b": true, // parents overlap too
		"sec_root": true, // doc-wide root overlaps every range
	}
	got := make(map[tree.SectionID]bool, len(res.SelectedIDs))
	for _, id := range res.SelectedIDs {
		got[id] = true
		if !wantSecs[id] {
			t.Errorf("unexpected section ID %q", id)
		}
	}
	// Leaves are the load-bearing requirement; parents are
	// allowed-not-required (a future tightening could skip them, and
	// the strategy contract stays useful either way).
	for _, id := range []tree.SectionID{"sec_a2", "sec_b2"} {
		if !got[id] {
			t.Errorf("missing section ID %q from SelectedIDs", id)
		}
	}
}

// TestTreeWalkMaxHopsForcesDone confirms a runaway loop is killed:
// the model emits get_pages on every turn but never done. The
// strategy must cap at MaxHops, force a done on the last hop, and
// surface a Result with HopsTaken == MaxHops+1 (the +1 for the
// forced terminal call).
func TestTreeWalkMaxHopsForcesDone(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		// Every loop reply is a fresh get_pages — never done.
		loopReply: `{"tool":"get_pages","start_page":1,"end_page":2}`,
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{"a1_ref": "install"}}
	s.MaxHops = 3

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.HopsTaken < 3 {
		t.Errorf("HopsTaken = %d, want >= 3 (cap hit)", res.HopsTaken)
	}
	// The model never emits done so even after force-done attempt
	// the answer should be empty (force-done's response is also a
	// get_pages, which fails to parse as done).
	if strings.TrimSpace(res.Reasoning) != "" {
		t.Errorf("answer must be empty when model never finalises, got %q", res.Reasoning)
	}
	// The get_pages calls that fired BEFORE the cap should still be
	// surfaced in PagesRead so callers can see what the model tried.
	if len(res.PagesRead) == 0 {
		t.Error("PagesRead must capture pre-cap navigation footprint")
	}
}

// TestTreeWalkMaxHopsForceDoneSucceeds covers the recovery path:
// the loop hit MaxHops, but on the forced-done turn the model
// actually emits a valid done. The strategy must collect the
// answer + citations from that final turn rather than dropping them.
func TestTreeWalkMaxHopsForceDoneSucceeds(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":1,"end_page":2}`,
			`{"tool":"get_pages","start_page":3,"end_page":4}`,
			// Once force-done fires, this becomes the model's response.
			`{"tool":"done","answer":"forced answer","cited_pages":[[1,2]]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{"a1_ref": "install", "a2_ref": "config"}}
	s.MaxHops = 2

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.Reasoning != "forced answer" {
		t.Errorf("forced-done answer = %q, want %q", res.Reasoning, "forced answer")
	}
	if len(res.SelectedIDs) == 0 {
		t.Error("forced-done citations must populate SelectedIDs")
	}
}

// TestTreeWalkTOCFallback exercises the graceful-degradation path:
// when the persisted TOC provider returns ErrNoTOC (pre-PR-A
// state), the strategy synthesises a TOC view from the section
// tree. The model must still receive section titles + page ranges.
func TestTreeWalkTOCFallback(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_document_structure"}`,
			`{"tool":"done","answer":"see structure","cited_pages":[]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}
	s.TOC = pageErroringTOC{} // mimic documents.toc_tree IS NULL

	res, err := s.SelectWithCost(context.Background(), tr, "what's in the doc?", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.HopsTaken != 2 {
		t.Errorf("HopsTaken = %d, want 2", res.HopsTaken)
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.lastPrompts) < 2 {
		t.Fatalf("expected 2 prompts, got %d", len(llm.lastPrompts))
	}
	// The fallback synthesised TOC must include each leaf title.
	obs := llm.lastPrompts[1]
	for _, want := range []string{"Install", "Config", "Querying", "Debt", "page_start"} {
		if !strings.Contains(obs, want) {
			t.Errorf("synthesised TOC missing %q in observation:\n%s", want, obs)
		}
	}
}

// TestTreeWalkTOCFromProvider asserts the persisted TOC wins over
// the synthesised view: when the provider returns bytes, those
// bytes are surfaced verbatim.
func TestTreeWalkTOCFromProvider(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_document_structure"}`,
			`{"tool":"done","answer":"from persisted TOC","cited_pages":[]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.TOC = pageStaticTOC{blob: []byte(`[{"title":"OVERRIDDEN","page_start":1,"page_end":99}]`)}

	_, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if !strings.Contains(llm.lastPrompts[1], "OVERRIDDEN") {
		t.Errorf("persisted TOC blob must be surfaced verbatim, got:\n%s", llm.lastPrompts[1])
	}
	if strings.Contains(llm.lastPrompts[1], "Install") {
		t.Errorf("persisted TOC should win — the synthesised one mustn't leak through")
	}
}

// TestTreeWalkBadJSONGraceful: persistent prose responses must
// trigger a retry prompt and then bail cleanly at MaxHops.
func TestTreeWalkBadJSONGraceful(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		loopReply: "I think the answer is on page 5.", // never JSON
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}
	s.MaxHops = 3

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("want nil error on persistent parse failure, got %v", err)
	}
	if strings.TrimSpace(res.Reasoning) != "" {
		t.Errorf("answer must be empty when no done emitted, got %q", res.Reasoning)
	}
	if len(res.PagesRead) != 0 {
		t.Errorf("PagesRead must be empty when every turn fails to parse, got %v", res.PagesRead)
	}
}

// TestTreeWalkClampInvalidRange: a model that asks for pages past
// the document's end gets a recoverable error observation and can
// keep going. The strategy must NOT crash on out-of-range input.
func TestTreeWalkClampInvalidRange(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree() // max page is 9
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":100,"end_page":105}`, // past the end
			`{"tool":"done","answer":"recovered","cited_pages":[[1,1]]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{"a1_ref": "install"}}

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.Reasoning != "recovered" {
		t.Errorf("recovery answer = %q, want %q", res.Reasoning, "recovered")
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	// The bad get_pages must surface "invalid range" so the model
	// has something to react to.
	if !strings.Contains(llm.lastPrompts[1], "invalid range") {
		t.Errorf("out-of-range get_pages should produce an 'invalid range' observation; got:\n%s", llm.lastPrompts[1])
	}
}

// TestTreeWalkClampPartialOverlap: a range that overlaps the
// document but extends past the end is silently clamped — the
// model gets useful content (not an error) for the in-range part.
func TestTreeWalkClampPartialOverlap(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree() // max page is 9
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":8,"end_page":50}`, // 8 is valid, 50 is past
			`{"tool":"done","answer":"got it","cited_pages":[[8,9]]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{"b2_ref": "Debt content."}}

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.PagesRead) != 1 {
		t.Fatalf("PagesRead = %v, want 1 entry", res.PagesRead)
	}
	if res.PagesRead[0].EndPage != 9 {
		t.Errorf("end page should be clamped to 9, got %d", res.PagesRead[0].EndPage)
	}
}

// TestTreeWalkEmptyTree exercises the early-return guard.
func TestTreeWalkEmptyTree(t *testing.T) {
	t.Parallel()

	llm := &pageScriptedLLM{}
	s := retrieval.NewTreeWalkStrategy(llm)

	res, err := s.SelectWithCost(context.Background(), &tree.Tree{}, "q", retrieval.ContextBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SelectedIDs) != 0 {
		t.Errorf("empty tree should yield empty selection, got %v", res.SelectedIDs)
	}
	if atomic.LoadInt32(&llm.calls) != 0 {
		t.Errorf("empty tree should make 0 LLM calls, got %d", llm.calls)
	}
}

// TestTreeWalkNoLoaderFallback: PageLoader=nil falls back to a
// title+summary rendering of get_pages. The model still gets a
// useful observation so it can keep navigating.
func TestTreeWalkNoLoaderFallback(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":1,"end_page":2}`,
			`{"tool":"done","answer":"titles only","cited_pages":[[1,2]]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm) // no PageLoader

	_, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	obs := llm.lastPrompts[1]
	if !strings.Contains(obs, "Install") || !strings.Contains(obs, "install steps") {
		t.Errorf("loader-less get_pages should fall back to title + summary; got:\n%s", obs)
	}
}

// TestTreeWalkContentClippedAtLimit: a get_pages call that would
// produce more chars than PageContentLimit must be clipped.
func TestTreeWalkContentClippedAtLimit(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	bigBody := strings.Repeat("X", 5_000)
	loader := pageMapLoader{data: map[string]string{
		"a1_ref": bigBody, "a2_ref": bigBody, "b1_ref": bigBody, "b2_ref": bigBody,
	}}
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":1,"end_page":9}`,
			`{"tool":"done","answer":"big","cited_pages":[[1,1]]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = loader
	s.PageContentLimit = 1000

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.PagesRead[0].CharCount > 1000 {
		t.Errorf("get_pages output must respect PageContentLimit=1000, got %d", res.PagesRead[0].CharCount)
	}
}

// TestTreeWalkNoCitationsClearsSelection: an empty cited_pages
// list must produce an empty SelectedIDs (no implicit "default to
// everything we visited"). This is the "no useful evidence found"
// path the system prompt prescribes.
func TestTreeWalkNoCitationsClearsSelection(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":1,"end_page":2}`,
			`{"tool":"done","answer":"The document does not address this query.","cited_pages":[]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{"a1_ref": "install"}}

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.SelectedIDs) != 0 {
		t.Errorf("empty cited_pages should yield empty SelectedIDs, got %v", res.SelectedIDs)
	}
	if !strings.Contains(res.Reasoning, "does not address") {
		t.Errorf("refusal answer must propagate to Reasoning, got %q", res.Reasoning)
	}
}

// TestTreeWalkTraceTokenStable: two runs that emit identical
// cited_pages produce identical trace tokens. Replay's substrate.
func TestTreeWalkTraceTokenStable(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	mkRun := func() string {
		llm := &pageScriptedLLM{
			replies: []string{
				`{"tool":"done","answer":"X","cited_pages":[[1,2],[8,9]]}`,
			},
		}
		s := retrieval.NewTreeWalkStrategy(llm)
		s.PageLoader = pageMapLoader{}
		res, _ := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{ModelName: "gpt-4o-mini"})
		return res.TraceToken
	}
	t1 := mkRun()
	t2 := mkRun()
	if t1 == "" || t1 != t2 {
		t.Errorf("trace tokens must be stable across runs; got %q vs %q", t1, t2)
	}
}

// TestTreeWalkTraceTokenOrderInvariant: two runs that cite the
// same pages in different orders must produce identical tokens.
func TestTreeWalkTraceTokenOrderInvariant(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	mkRun := func(reply string) string {
		llm := &pageScriptedLLM{replies: []string{reply}}
		s := retrieval.NewTreeWalkStrategy(llm)
		res, _ := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{ModelName: "gpt-4o-mini"})
		return res.TraceToken
	}
	t1 := mkRun(`{"tool":"done","answer":"X","cited_pages":[[1,2],[8,9]]}`)
	t2 := mkRun(`{"tool":"done","answer":"X","cited_pages":[[8,9],[1,2]]}`)
	if t1 != t2 {
		t.Errorf("trace tokens must be order-invariant; got %q vs %q", t1, t2)
	}
}

// TestParseTreeWalkActionTolerance covers the input shapes the
// parser accepts:
//   - "tool" key (canonical)
//   - "action" key (alt)
//   - "pages":"5-7" string
//   - cited_pages as string list ["5-7","10"]
//   - markdown fences + prose prefix
//   - case-insensitive tool tag
func TestParseTreeWalkActionTolerance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		tool  string
		start int
		end   int
		cited [][2]int
	}{
		{
			name: "canonical_structure",
			in:   `{"tool":"get_document_structure","reasoning":"orient"}`,
			tool: "get_document_structure",
		},
		{
			name:  "canonical_pages",
			in:    `{"tool":"get_pages","start_page":5,"end_page":7}`,
			tool:  "get_pages",
			start: 5, end: 7,
		},
		{
			name:  "alt_action_key",
			in:    `{"action":"get_pages","start_page":5,"end_page":7}`,
			tool:  "get_pages",
			start: 5, end: 7,
		},
		{
			name:  "pages_string_range",
			in:    `{"tool":"get_pages","pages":"5-7"}`,
			tool:  "get_pages",
			start: 5, end: 7,
		},
		{
			name:  "pages_string_single",
			in:    `{"tool":"get_pages","pages":"12"}`,
			tool:  "get_pages",
			start: 12, end: 12,
		},
		{
			name:  "code_fence",
			in:    "```json\n{\"tool\":\"get_pages\",\"start_page\":3,\"end_page\":4}\n```",
			tool:  "get_pages",
			start: 3, end: 4,
		},
		{
			name:  "prose_before",
			in:    `Sure: {"tool":"get_pages","start_page":1,"end_page":1}`,
			tool:  "get_pages",
			start: 1, end: 1,
		},
		{
			name:  "case_insensitive",
			in:    `{"tool":"GET_PAGES","start_page":2,"end_page":3}`,
			tool:  "get_pages",
			start: 2, end: 3,
		},
		{
			name:  "done_with_citations",
			in:    `{"tool":"done","answer":"hi","cited_pages":[[1,2],[5,7]]}`,
			tool:  "done",
			cited: [][2]int{{1, 2}, {5, 7}},
		},
		{
			name:  "done_with_string_citations",
			in:    `{"tool":"done","answer":"hi","cited_pages":["1-2","5-7"]}`,
			tool:  "done",
			cited: [][2]int{{1, 2}, {5, 7}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := retrieval.ParseTreeWalkAction(c.in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got.Action != c.tool {
				t.Errorf("Action = %q, want %q", got.Action, c.tool)
			}
			if got.StartPage != c.start {
				t.Errorf("StartPage = %d, want %d", got.StartPage, c.start)
			}
			if got.EndPage != c.end {
				t.Errorf("EndPage = %d, want %d", got.EndPage, c.end)
			}
			if len(got.CitedPages) != len(c.cited) {
				t.Fatalf("CitedPages len = %d, want %d (got %v)", len(got.CitedPages), len(c.cited), got.CitedPages)
			}
			for i := range c.cited {
				if got.CitedPages[i] != c.cited[i] {
					t.Errorf("CitedPages[%d] = %v, want %v", i, got.CitedPages[i], c.cited[i])
				}
			}
		})
	}
}

func TestParseTreeWalkActionRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		"I think it's page 5.",
		`{"reasoning":"no tool field"}`,
	} {
		_, err := retrieval.ParseTreeWalkAction(in)
		if err == nil {
			t.Errorf("want error parsing %q", in)
		}
	}
}

// indexOfSection is a tiny helper that says "is needle in haystack
// and where". Mirrors slices.Index for readability — keeps the tests
// stdlib-agnostic on older Go versions.
func indexOfSection(haystack []tree.SectionID, needle tree.SectionID) (int, bool) {
	for i, id := range haystack {
		if id == needle {
			return i, true
		}
	}
	return -1, false
}

// distinctRangeCount counts the unique [start,end] pairs in a
// CitedPages slice. The whole point of the dedup work is that this
// equals len(CitedPages) — no pair appears twice.
func distinctRangeCount(pairs [][2]int) int {
	seen := map[[2]int]struct{}{}
	for _, p := range pairs {
		seen[p] = struct{}{}
	}
	return len(seen)
}

// TestTreeWalkDedupCollapsesRepeatedRange is the core regression for
// the FinanceBench "same id ×5" miss (id_00499 returned sec_363... five
// times). A done that cites the SAME range five times, plus two more
// distinct ranges, must collapse to distinct ranges only AND respect
// the MaxCitations cap. With MaxCitations=3 the three distinct ranges
// survive; the four duplicate repeats are gone.
func TestTreeWalkDedupCollapsesRepeatedRange(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree() // pages 1-9
	llm := &pageScriptedLLM{
		replies: []string{
			// sec_a1 (pages 1-2) cited five times, then two distinct
			// ranges. This is the spray-with-dupes pattern.
			`{"tool":"done","answer":"sprayed","cited_pages":[[1,2],[1,2],[1,2],[1,2],[1,2],[3,4],[8,9]]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}
	// Default MaxCitations is 3.

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}

	// CitedPages must be deduped: the [1,2] repeat collapses to one,
	// and the total distinct set is capped at 3.
	if n := distinctRangeCount(res.CitedPages); n != len(res.CitedPages) {
		t.Errorf("CitedPages contains duplicates: %v (distinct=%d, len=%d)", res.CitedPages, n, len(res.CitedPages))
	}
	if len(res.CitedPages) > 3 {
		t.Errorf("CitedPages must be capped at MaxCitations=3, got %d: %v", len(res.CitedPages), res.CitedPages)
	}
	// The three distinct ranges [1,2],[3,4],[8,9] all survive (each is
	// under the cap), in canonical page order.
	want := [][2]int{{1, 2}, {3, 4}, {8, 9}}
	if len(res.CitedPages) != len(want) {
		t.Fatalf("CitedPages = %v, want %v", res.CitedPages, want)
	}
	for i := range want {
		if res.CitedPages[i] != want[i] {
			t.Errorf("CitedPages[%d] = %v, want %v", i, res.CitedPages[i], want[i])
		}
	}

	// SelectedIDs must also be deduped — sec_a1 must appear exactly
	// once even though it was cited five times. This is the precision
	// fix: no section id repeats.
	count := map[tree.SectionID]int{}
	for _, id := range res.SelectedIDs {
		count[id]++
	}
	for id, c := range count {
		if c > 1 {
			t.Errorf("section id %q appears %d times in SelectedIDs; must be deduped", id, c)
		}
	}
}

// TestTreeWalkCapKeepsHighestConfidence proves the cap is
// confidence-aware: when more than MaxCitations distinct ranges are
// cited WITH per-range confidence, the highest-confidence ranges win
// (not the first-listed). Here five ranges are cited; the cap is 2;
// the two with the top confidence must be the ones kept.
func TestTreeWalkCapKeepsHighestConfidence(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree() // pages 1-9
	// Rich cited_pages with per-range confidence. The two strongest
	// are [8,9] (0.95) and [5,7] (0.9); the cap of 2 should keep those.
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"done","answer":"x","confidence":0.9,"cited_pages":[` +
				`{"pages":[1,2],"confidence":0.1},` +
				`{"pages":[3,4],"confidence":0.2},` +
				`{"pages":[5,7],"confidence":0.9},` +
				`{"pages":[8,9],"confidence":0.95},` +
				`{"pages":[1,1],"confidence":0.05}]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}
	s.MaxCitations = 2

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.CitedPages) != 2 {
		t.Fatalf("CitedPages must be capped at 2, got %d: %v", len(res.CitedPages), res.CitedPages)
	}
	// Output is page-sorted, so [5,7] then [8,9].
	want := [][2]int{{5, 7}, {8, 9}}
	for i := range want {
		if res.CitedPages[i] != want[i] {
			t.Errorf("CitedPages[%d] = %v, want %v (the two highest-confidence ranges)", i, res.CitedPages[i], want[i])
		}
	}
	// Overall confidence (0.9) must surface on the Result.
	if res.Confidence != 0.9 {
		t.Errorf("Result.Confidence = %v, want 0.9", res.Confidence)
	}
}

// TestTreeWalkConfidentSinglePreserved is the happy half of the
// signal: a confident single citation must pass through untouched and
// the confidence must surface on both Result.Confidence and the
// per-section Confidences map (so the abstain machinery can read it).
func TestTreeWalkConfidentSinglePreserved(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"done","answer":"Install on pages 1-2.","cited_pages":[[1,2]],"confidence":0.92,"reasoning":"clear"}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}

	res, err := s.SelectWithCost(context.Background(), tr, "how do I install?", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.CitedPages) != 1 || res.CitedPages[0] != [2]int{1, 2} {
		t.Errorf("single confident citation must be preserved, got %v", res.CitedPages)
	}
	if res.Confidence != 0.92 {
		t.Errorf("Result.Confidence = %v, want 0.92", res.Confidence)
	}
	if len(res.Confidences) == 0 {
		t.Fatal("Confidences map must surface the answer confidence per selected section")
	}
	// sec_a1 overlaps pages 1-2 and must carry the confidence.
	if got := res.Confidences["sec_a1"]; got != 0.92 {
		t.Errorf("Confidences[sec_a1] = %v, want 0.92", got)
	}
}

// TestTreeWalkLowConfidenceStillCommitsSingle guards the over-
// suppression risk: even when the model reports LOW confidence, it must
// still return its single best pick — never an empty citation set. A
// low confidence annotates the answer; it does not delete it.
func TestTreeWalkLowConfidenceStillCommitsSingle(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"done","answer":"Probably debt on 8-9.","cited_pages":[[8,9]],"confidence":0.15,"reasoning":"unsure"}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.CitedPages) != 1 || res.CitedPages[0] != [2]int{8, 9} {
		t.Errorf("low-confidence answer must still commit to its single pick, got %v", res.CitedPages)
	}
	if res.Confidence != 0.15 {
		t.Errorf("Result.Confidence = %v, want 0.15 (low but surfaced)", res.Confidence)
	}
	if len(res.SelectedIDs) == 0 {
		t.Error("low confidence must not empty the selection")
	}
}

// TestTreeWalkConfidenceClamped: out-of-range confidence values clamp
// into [0,1] rather than propagating absurd numbers.
func TestTreeWalkConfidenceClamped(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"done","answer":"x","cited_pages":[[1,2]],"confidence":7.5}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if res.Confidence != 1.0 {
		t.Errorf("confidence 7.5 must clamp to 1.0, got %v", res.Confidence)
	}
}

// TestTreeWalkCapConfigurable: a custom MaxCitations is honoured. Six
// distinct ranges cited, cap=1 → exactly one survives.
func TestTreeWalkCapConfigurable(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"done","answer":"x","cited_pages":[[1,1],[2,2],[3,3],[4,4],[5,5],[6,6]]}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}
	s.MaxCitations = 1

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.CitedPages) != 1 {
		t.Errorf("MaxCitations=1 must keep exactly one range, got %d: %v", len(res.CitedPages), res.CitedPages)
	}
	// With no per-range confidence the cap falls back to emission
	// order: the first-listed range [1,1] wins.
	if res.CitedPages[0] != [2]int{1, 1} {
		t.Errorf("cap with no confidence should keep first-listed range [1,1], got %v", res.CitedPages[0])
	}
}

// TestTreeWalkEmptyCitationsNoConfidence: a refusal (empty cited_pages)
// leaves CitedPages nil, Confidences nil, and Confidence 0 — so the
// API layer's abstain check (which fires only on a non-empty
// Confidences map) behaves exactly as before for refusals.
func TestTreeWalkEmptyCitationsNoConfidence(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"done","answer":"The document does not address this query.","cited_pages":[],"confidence":0}`,
		},
	}
	s := retrieval.NewTreeWalkStrategy(llm)
	s.PageLoader = pageMapLoader{data: map[string]string{}}

	res, err := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{MaxTokens: 100000})
	if err != nil {
		t.Fatalf("SelectWithCost: %v", err)
	}
	if len(res.CitedPages) != 0 {
		t.Errorf("refusal must cite nothing, got %v", res.CitedPages)
	}
	if len(res.Confidences) != 0 {
		t.Errorf("no confidence map on a zero-confidence refusal, got %v", res.Confidences)
	}
	if len(res.SelectedIDs) != 0 {
		t.Errorf("refusal must select nothing, got %v", res.SelectedIDs)
	}
}

// TestParseTreeWalkConfidenceAndRichCitations covers the new parser
// surfaces: a top-level confidence number, and the rich cited_pages
// object form carrying per-range confidence.
func TestParseTreeWalkConfidenceAndRichCitations(t *testing.T) {
	t.Parallel()

	t.Run("top_level_confidence", func(t *testing.T) {
		a, err := retrieval.ParseTreeWalkAction(`{"tool":"done","answer":"x","cited_pages":[[1,2]],"confidence":0.8}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if a.Confidence != 0.8 {
			t.Errorf("Confidence = %v, want 0.8", a.Confidence)
		}
	})

	t.Run("confidence_as_string", func(t *testing.T) {
		a, err := retrieval.ParseTreeWalkAction(`{"tool":"done","answer":"x","cited_pages":[[1,2]],"confidence":"0.6"}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if a.Confidence != 0.6 {
			t.Errorf("Confidence = %v, want 0.6 (string form)", a.Confidence)
		}
	})

	t.Run("rich_cited_pages_objects", func(t *testing.T) {
		a, err := retrieval.ParseTreeWalkAction(`{"tool":"done","answer":"x","cited_pages":[{"pages":[5,7],"confidence":0.9},{"pages":[12,12],"confidence":0.4}]}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		want := [][2]int{{5, 7}, {12, 12}}
		if len(a.CitedPages) != len(want) {
			t.Fatalf("CitedPages = %v, want %v", a.CitedPages, want)
		}
		for i := range want {
			if a.CitedPages[i] != want[i] {
				t.Errorf("CitedPages[%d] = %v, want %v", i, a.CitedPages[i], want[i])
			}
		}
	})

	t.Run("rich_cited_pages_start_end", func(t *testing.T) {
		a, err := retrieval.ParseTreeWalkAction(`{"tool":"done","answer":"x","cited_pages":[{"start":3,"end":4,"confidence":0.7}]}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(a.CitedPages) != 1 || a.CitedPages[0] != [2]int{3, 4} {
			t.Errorf("CitedPages = %v, want [[3 4]]", a.CitedPages)
		}
	})
}
