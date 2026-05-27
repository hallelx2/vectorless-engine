package ingest

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// scriptedLLM is a minimal inline mock — kept inside the ingest
// package so it doesn't leak into the public API surface. Each
// call walks a script keyed by phase ("detect", "extract",
// "verify"), returning the next canned response. Mirrors the
// pattern used in pkg/retrieval/retrieval_test.go's mockLLM but
// scoped narrower so individual tests can wire bespoke behaviour
// without dragging the retrieval test fixture in.
type scriptedLLM struct {
	mu    sync.Mutex
	calls int32

	// route returns the response for a given prompt. Tests inject
	// behaviour here; falls back to a permissive "no" detector +
	// empty extractor when nil so unrelated test paths don't have
	// to script every prompt.
	route func(userPrompt string) string

	// captured holds every user prompt seen, in order. Tests
	// assert phase ordering and prompt content from this.
	captured []string
}

func (m *scriptedLLM) Complete(_ context.Context, req llmgate.Request) (*llmgate.Response, error) {
	atomic.AddInt32(&m.calls, 1)
	var user string
	for _, msg := range req.Messages {
		if msg.Role == llmgate.RoleUser {
			user = msg.Content
		}
	}
	m.mu.Lock()
	m.captured = append(m.captured, user)
	m.mu.Unlock()

	content := ""
	if m.route != nil {
		content = m.route(user)
	}
	if content == "" {
		content = `{"toc_detected":"no"}`
	}
	return &llmgate.Response{
		Content: content,
		Usage:   llmgate.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}, nil
}

func (m *scriptedLLM) CountTokens(_ context.Context, s string) (int, error) {
	return len(s) / 4, nil
}

// TestBuildTOCFoundPath walks the happy path where the detector
// finds a TOC page, the extractor parses it into nested nodes,
// and verification leaves the start pages intact.
func TestBuildTOCFoundPath(t *testing.T) {
	llm := &scriptedLLM{}
	llm.route = func(prompt string) string {
		switch {
		case strings.Contains(prompt, "table of contents provided in the given text"):
			// Detector: yes only when the page actually contains
			// "Table of Contents".
			if strings.Contains(prompt, "Table of Contents") {
				return `{"toc_detected":"yes"}`
			}
			return `{"toc_detected":"no"}`
		case strings.Contains(prompt, "hierarchical tree structure"):
			// Extractor: return a small 10-K outline.
			return `{"nodes":[
				{"structure":"1","title":"Business","physical_index":"<physical_index_3>"},
				{"structure":"1.1","title":"Overview","physical_index":"<physical_index_3>"},
				{"structure":"2","title":"Risk Factors","physical_index":"<physical_index_10>"},
				{"structure":"3","title":"MD&A","physical_index":"<physical_index_20>"}
			]}`
		case strings.Contains(prompt, "section starts at the beginning"):
			return `{"start_begin":"yes"}`
		}
		return `{"toc_detected":"no"}`
	}

	pages := []PageText{
		{PageNumber: 1, Text: "Cover Page\nForm 10-K\n"},
		{PageNumber: 2, Text: "Table of Contents\n1. Business ... 3\n1.1 Overview ... 3\n2. Risk Factors ... 10\n3. MD&A ... 20"},
		{PageNumber: 3, Text: "Business\nWe are a company that does things."},
		{PageNumber: 10, Text: "Risk Factors\nVarious risks apply."},
		{PageNumber: 20, Text: "MD&A\nDiscussion of operations."},
	}

	b := &TOCBuilder{LLM: llm, TOCCheckPages: 5, Concurrency: 2}
	nodes, usage, err := b.Build(context.Background(), pages)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("top-level nodes: got %d want 3 (Business, Risk Factors, MD&A) — got: %+v", len(nodes), nodes)
	}
	if nodes[0].Title != "Business" || nodes[0].StartPage != 3 {
		t.Errorf("nodes[0]: got %+v", nodes[0])
	}
	if len(nodes[0].Nodes) != 1 || nodes[0].Nodes[0].Title != "Overview" {
		t.Errorf("nodes[0].Nodes: got %+v", nodes[0].Nodes)
	}
	if nodes[1].Title != "Risk Factors" || nodes[1].StartPage != 10 {
		t.Errorf("nodes[1]: got %+v", nodes[1])
	}
	if nodes[1].EndPage != 19 {
		t.Errorf("nodes[1].EndPage: got %d want 19 (one before MD&A's start)", nodes[1].EndPage)
	}
	if nodes[2].EndPage != 20 {
		t.Errorf("nodes[2].EndPage (last sibling): got %d want 20 (doc last page)", nodes[2].EndPage)
	}
	if usage.LLMCalls < 2 {
		t.Errorf("expected at least 2 LLM calls (detector + extractor), got %d", usage.LLMCalls)
	}
	if usage.InputTokens == 0 {
		t.Errorf("usage should track input tokens; got 0")
	}
	// NodeIDs are stamped deterministically from structure.
	if nodes[0].NodeID != "toc_1" || nodes[0].Nodes[0].NodeID != "toc_1_1.1" {
		t.Errorf("node IDs not stamped: top=%q child=%q", nodes[0].NodeID, nodes[0].Nodes[0].NodeID)
	}
}

// TestBuildNoTOCPath drives the generateTOCInit branch — the
// detector replies "no" for every page, so the builder falls
// through to the body-text TOC generator.
func TestBuildNoTOCPath(t *testing.T) {
	llm := &scriptedLLM{}
	var extractorCalled atomic.Int32
	var noTOCCalled atomic.Int32
	llm.route = func(prompt string) string {
		switch {
		case strings.Contains(prompt, "table of contents provided in the given text"):
			return `{"toc_detected":"no"}`
		case strings.Contains(prompt, "hierarchical tree structure"):
			// The no-TOC and extractor prompts share the same
			// system prompt + JSON shape; the user prompt body
			// differs. We distinguish by the "raw table of
			// contents" marker which only the extractor uses.
			if strings.Contains(prompt, "Raw table of contents") {
				extractorCalled.Add(1)
			} else {
				noTOCCalled.Add(1)
			}
			return `{"nodes":[
				{"structure":"1","title":"Introduction","physical_index":"<physical_index_2>"},
				{"structure":"2","title":"Methods","physical_index":"<physical_index_5>"}
			]}`
		case strings.Contains(prompt, "section starts at the beginning"):
			return `{"start_begin":"yes"}`
		}
		return `{"toc_detected":"no"}`
	}

	pages := []PageText{
		{PageNumber: 1, Text: "Cover page with no TOC."},
		{PageNumber: 2, Text: "Introduction\nWe study X."},
		{PageNumber: 5, Text: "Methods\nWe used Y."},
	}

	b := &TOCBuilder{LLM: llm, TOCCheckPages: 5, Concurrency: 2}
	nodes, _, err := b.Build(context.Background(), pages)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if extractorCalled.Load() != 0 {
		t.Errorf("extractor should NOT run when no TOC page was detected")
	}
	if noTOCCalled.Load() == 0 {
		t.Errorf("no-TOC generator should have been invoked")
	}
	if len(nodes) != 2 || nodes[0].Title != "Introduction" || nodes[1].Title != "Methods" {
		t.Fatalf("got nodes %+v", nodes)
	}
	if nodes[0].StartPage != 2 || nodes[1].StartPage != 5 {
		t.Errorf("page numbers not lifted from <physical_index>: got %+v", nodes)
	}
}

// TestVerificationRepairsWrongPage scripts a verifier that says
// "no" for a node whose claimed page doesn't match — the start
// page should be cleared back to zero. Downstream consumers treat
// zero as "open / unknown" rather than a lie.
func TestVerificationRepairsWrongPage(t *testing.T) {
	llm := &scriptedLLM{}
	llm.route = func(prompt string) string {
		switch {
		case strings.Contains(prompt, "table of contents provided"):
			if strings.Contains(prompt, "Table of Contents") {
				return `{"toc_detected":"yes"}`
			}
			return `{"toc_detected":"no"}`
		case strings.Contains(prompt, "hierarchical tree structure"):
			return `{"nodes":[
				{"structure":"1","title":"Foo","physical_index":"<physical_index_4>"},
				{"structure":"2","title":"Bar","physical_index":"<physical_index_7>"}
			]}`
		case strings.Contains(prompt, "section starts at the beginning"):
			// Only Foo's claim is valid; Bar's is a lie.
			if strings.Contains(prompt, "Section title: Foo") {
				return `{"start_begin":"yes"}`
			}
			return `{"start_begin":"no"}`
		}
		return `{"toc_detected":"no"}`
	}

	pages := []PageText{
		{PageNumber: 1, Text: "Table of Contents\nFoo ... 4\nBar ... 7"},
		{PageNumber: 4, Text: "Foo\nbody of foo"},
		{PageNumber: 7, Text: "Some other content, not Bar"},
	}

	b := &TOCBuilder{LLM: llm, TOCCheckPages: 5, Concurrency: 2}
	nodes, _, err := b.Build(context.Background(), pages)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes: got %d want 2", len(nodes))
	}
	if nodes[0].StartPage != 4 {
		t.Errorf("verified node Foo should keep page 4, got %d", nodes[0].StartPage)
	}
	if nodes[1].StartPage != 0 {
		t.Errorf("repaired node Bar should have StartPage=0 (cleared), got %d", nodes[1].StartPage)
	}
}

// TestRetryOnBadJSON exercises the retry path: the first
// extractor response is plain prose, the second is valid JSON.
// The builder should retry and end up with usable nodes.
func TestRetryOnBadJSON(t *testing.T) {
	llm := &scriptedLLM{}
	var extractorCalls atomic.Int32
	llm.route = func(prompt string) string {
		if strings.Contains(prompt, "table of contents provided") {
			if strings.Contains(prompt, "Table of Contents") {
				return `{"toc_detected":"yes"}`
			}
			return `{"toc_detected":"no"}`
		}
		if strings.Contains(prompt, "hierarchical tree structure") {
			n := extractorCalls.Add(1)
			if n == 1 {
				// First try: plain prose. Retry loop should kick in.
				return "Sure, here is the structure: I will explain it ..."
			}
			return `{"nodes":[{"structure":"1","title":"Solo","physical_index":"<physical_index_2>"}]}`
		}
		if strings.Contains(prompt, "section starts at the beginning") {
			return `{"start_begin":"yes"}`
		}
		return `{"toc_detected":"no"}`
	}

	pages := []PageText{
		{PageNumber: 1, Text: "Table of Contents\nSolo ... 2"},
		{PageNumber: 2, Text: "Solo\nbody"},
	}

	b := &TOCBuilder{LLM: llm, TOCCheckPages: 5, Concurrency: 2}
	nodes, usage, err := b.Build(context.Background(), pages)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Title != "Solo" {
		t.Fatalf("nodes: %+v", nodes)
	}
	if extractorCalls.Load() < 2 {
		t.Errorf("expected the retry loop to fire (>=2 extractor calls), got %d", extractorCalls.Load())
	}
	// Retry adds an extra LLM call beyond the minimum (detector + extractor + verify).
	if usage.LLMCalls < 4 {
		t.Errorf("expected >=4 LLM calls (detector + extractor*2 + verify), got %d", usage.LLMCalls)
	}
}

// TestEndPageDerivationFromSiblings asserts the post-verification
// pass fills EndPage from the next sibling's StartPage - 1 and
// the document's last page for the final sibling.
func TestEndPageDerivationFromSiblings(t *testing.T) {
	root := []tree.TOCNode{
		{Structure: "1", Title: "A", StartPage: 5},
		{Structure: "2", Title: "B", StartPage: 12},
		{Structure: "3", Title: "C", StartPage: 30},
	}
	deriveEndPages(root, 50)
	if root[0].EndPage != 11 {
		t.Errorf("A.EndPage: got %d want 11", root[0].EndPage)
	}
	if root[1].EndPage != 29 {
		t.Errorf("B.EndPage: got %d want 29", root[1].EndPage)
	}
	if root[2].EndPage != 50 {
		t.Errorf("C.EndPage (last): got %d want 50", root[2].EndPage)
	}
}

// TestAssembleHierarchyNestsByStructure makes sure dotted
// structure indices group correctly. "1.1" nests under "1",
// "2.1.1" three levels deep, etc.
func TestAssembleHierarchyNestsByStructure(t *testing.T) {
	flat := []tocNodePayload{
		{Structure: "1", Title: "Top"},
		{Structure: "1.1", Title: "Sub-A"},
		{Structure: "1.1.1", Title: "Leaf-1"},
		{Structure: "1.2", Title: "Sub-B"},
		{Structure: "2", Title: "Sibling"},
	}
	out := assembleHierarchy(flat)
	if len(out) != 2 {
		t.Fatalf("top-level: got %d want 2", len(out))
	}
	if out[0].Title != "Top" || len(out[0].Nodes) != 2 {
		t.Fatalf("Top children: %+v", out[0].Nodes)
	}
	if out[0].Nodes[0].Title != "Sub-A" || len(out[0].Nodes[0].Nodes) != 1 {
		t.Fatalf("Sub-A: %+v", out[0].Nodes[0])
	}
	if out[0].Nodes[0].Nodes[0].Title != "Leaf-1" {
		t.Errorf("Leaf-1 missing under Sub-A; got %+v", out[0].Nodes[0].Nodes)
	}
}

// TestParsePhysicalIndex covers the tag parser used by
// assembleHierarchy. Malformed tags should return 0 (the
// "unknown" sentinel) rather than panic.
func TestParsePhysicalIndex(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"<physical_index_5>", 5},
		{"<physical_index_42>", 42},
		{"<physical_index_>", 0},
		{"not a tag", 0},
		{"<physical_index_abc>", 0},
		{"  <physical_index_7>  ", 7},
	}
	for _, c := range cases {
		if got := parsePhysicalIndex(c.in); got != c.want {
			t.Errorf("parsePhysicalIndex(%q): got %d want %d", c.in, got, c.want)
		}
	}
}

// TestBuildEmptyPages should return cleanly with no usage and no nodes.
func TestBuildEmptyPages(t *testing.T) {
	b := &TOCBuilder{LLM: &scriptedLLM{}}
	nodes, usage, err := b.Build(context.Background(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if nodes != nil {
		t.Errorf("empty input should yield nil nodes, got %v", nodes)
	}
	if usage.LLMCalls != 0 {
		t.Errorf("empty input should make no LLM calls, got %d", usage.LLMCalls)
	}
}
