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

// pageScriptedLLM is a scriptedLLM for the PageIndex strategy.
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
// page_end on every section so PageIndexStrategy can navigate. The
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

// TestPageIndexHappyPath drives the canonical 3-tool sequence:
// structure → get_pages → done. We assert the strategy:
//   - returns the answer string in Result.Reasoning
//   - lists the section IDs whose page range overlaps the citation
//   - records the get_pages call in PagesRead
//   - tracks HopsTaken correctly
//   - computes a non-empty TraceToken keyed by the cited pages
func TestPageIndexHappyPath(t *testing.T) {
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

	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexMinimalIngestedDoc is the cross-package guarantee for the
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
func TestPageIndexMinimalIngestedDoc(t *testing.T) {
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

	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexMultiRangeDone covers a done with two cited ranges:
// the strategy must surface every section that overlaps EITHER
// range. This is the FinanceBench-shaped pattern: an answer that
// pulls evidence from two unrelated parts of a 10-K.
func TestPageIndexMultiRangeDone(t *testing.T) {
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
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexMaxHopsForcesDone confirms a runaway loop is killed:
// the model emits get_pages on every turn but never done. The
// strategy must cap at MaxHops, force a done on the last hop, and
// surface a Result with HopsTaken == MaxHops+1 (the +1 for the
// forced terminal call).
func TestPageIndexMaxHopsForcesDone(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		// Every loop reply is a fresh get_pages — never done.
		loopReply: `{"tool":"get_pages","start_page":1,"end_page":2}`,
	}
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexMaxHopsForceDoneSucceeds covers the recovery path:
// the loop hit MaxHops, but on the forced-done turn the model
// actually emits a valid done. The strategy must collect the
// answer + citations from that final turn rather than dropping them.
func TestPageIndexMaxHopsForceDoneSucceeds(t *testing.T) {
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
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexTOCFallback exercises the graceful-degradation path:
// when the persisted TOC provider returns ErrNoTOC (pre-PR-A
// state), the strategy synthesises a TOC view from the section
// tree. The model must still receive section titles + page ranges.
func TestPageIndexTOCFallback(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_document_structure"}`,
			`{"tool":"done","answer":"see structure","cited_pages":[]}`,
		},
	}
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexTOCFromProvider asserts the persisted TOC wins over
// the synthesised view: when the provider returns bytes, those
// bytes are surfaced verbatim.
func TestPageIndexTOCFromProvider(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_document_structure"}`,
			`{"tool":"done","answer":"from persisted TOC","cited_pages":[]}`,
		},
	}
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexBadJSONGraceful: persistent prose responses must
// trigger a retry prompt and then bail cleanly at MaxHops.
func TestPageIndexBadJSONGraceful(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		loopReply: "I think the answer is on page 5.", // never JSON
	}
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexClampInvalidRange: a model that asks for pages past
// the document's end gets a recoverable error observation and can
// keep going. The strategy must NOT crash on out-of-range input.
func TestPageIndexClampInvalidRange(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree() // max page is 9
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":100,"end_page":105}`, // past the end
			`{"tool":"done","answer":"recovered","cited_pages":[[1,1]]}`,
		},
	}
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexClampPartialOverlap: a range that overlaps the
// document but extends past the end is silently clamped — the
// model gets useful content (not an error) for the in-range part.
func TestPageIndexClampPartialOverlap(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree() // max page is 9
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":8,"end_page":50}`, // 8 is valid, 50 is past
			`{"tool":"done","answer":"got it","cited_pages":[[8,9]]}`,
		},
	}
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexEmptyTree exercises the early-return guard.
func TestPageIndexEmptyTree(t *testing.T) {
	t.Parallel()

	llm := &pageScriptedLLM{}
	s := retrieval.NewPageIndexStrategy(llm)

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

// TestPageIndexNoLoaderFallback: PageLoader=nil falls back to a
// title+summary rendering of get_pages. The model still gets a
// useful observation so it can keep navigating.
func TestPageIndexNoLoaderFallback(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":1,"end_page":2}`,
			`{"tool":"done","answer":"titles only","cited_pages":[[1,2]]}`,
		},
	}
	s := retrieval.NewPageIndexStrategy(llm) // no PageLoader

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

// TestPageIndexContentClippedAtLimit: a get_pages call that would
// produce more chars than PageContentLimit must be clipped.
func TestPageIndexContentClippedAtLimit(t *testing.T) {
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
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexNoCitationsClearsSelection: an empty cited_pages
// list must produce an empty SelectedIDs (no implicit "default to
// everything we visited"). This is the "no useful evidence found"
// path the system prompt prescribes.
func TestPageIndexNoCitationsClearsSelection(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	llm := &pageScriptedLLM{
		replies: []string{
			`{"tool":"get_pages","start_page":1,"end_page":2}`,
			`{"tool":"done","answer":"The document does not address this query.","cited_pages":[]}`,
		},
	}
	s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexTraceTokenStable: two runs that emit identical
// cited_pages produce identical trace tokens. Replay's substrate.
func TestPageIndexTraceTokenStable(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	mkRun := func() string {
		llm := &pageScriptedLLM{
			replies: []string{
				`{"tool":"done","answer":"X","cited_pages":[[1,2],[8,9]]}`,
			},
		}
		s := retrieval.NewPageIndexStrategy(llm)
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

// TestPageIndexTraceTokenOrderInvariant: two runs that cite the
// same pages in different orders must produce identical tokens.
func TestPageIndexTraceTokenOrderInvariant(t *testing.T) {
	t.Parallel()

	tr := buildPagedTree()
	mkRun := func(reply string) string {
		llm := &pageScriptedLLM{replies: []string{reply}}
		s := retrieval.NewPageIndexStrategy(llm)
		res, _ := s.SelectWithCost(context.Background(), tr, "q", retrieval.ContextBudget{ModelName: "gpt-4o-mini"})
		return res.TraceToken
	}
	t1 := mkRun(`{"tool":"done","answer":"X","cited_pages":[[1,2],[8,9]]}`)
	t2 := mkRun(`{"tool":"done","answer":"X","cited_pages":[[8,9],[1,2]]}`)
	if t1 != t2 {
		t.Errorf("trace tokens must be order-invariant; got %q vs %q", t1, t2)
	}
}

// TestParsePageIndexActionTolerance covers the input shapes the
// parser accepts:
//   - "tool" key (canonical)
//   - "action" key (alt)
//   - "pages":"5-7" string
//   - cited_pages as string list ["5-7","10"]
//   - markdown fences + prose prefix
//   - case-insensitive tool tag
func TestParsePageIndexActionTolerance(t *testing.T) {
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
			got, err := retrieval.ParsePageIndexAction(c.in)
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

func TestParsePageIndexActionRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		"I think it's page 5.",
		`{"reasoning":"no tool field"}`,
	} {
		_, err := retrieval.ParsePageIndexAction(in)
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
