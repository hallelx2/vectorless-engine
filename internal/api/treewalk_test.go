package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/config"
	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// treeWalkScriptedLLM is the same shape as the strategy test's
// scripted LLM but mirrored here so the api package's tests don't
// reach into pkg/retrieval's test file.
type treeWalkScriptedLLM struct {
	replies []string
	calls   int32
}

func (p *treeWalkScriptedLLM) Complete(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
	i := int(atomic.AddInt32(&p.calls, 1)) - 1
	if i >= len(p.replies) {
		return nil, fmt.Errorf("scripted LLM exhausted at call %d", i+1)
	}
	return &llmgate.Response{Content: p.replies[i]}, nil
}

func (p *treeWalkScriptedLLM) CountTokens(ctx context.Context, t string) (int, error) {
	return len(t) / 4, nil
}

// inMemoryStorage is a minimal storage.Storage backed by a map.
// Only Get is meaningful for the treewalk handler tests.
type inMemoryStorage struct {
	data map[string][]byte
}

func (m *inMemoryStorage) Put(ctx context.Context, key string, r io.Reader, meta storage.Metadata) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[key] = b
	return nil
}

func (m *inMemoryStorage) Get(ctx context.Context, key string) (io.ReadCloser, storage.Metadata, error) {
	b, ok := m.data[key]
	if !ok {
		return nil, storage.Metadata{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), storage.Metadata{Key: key, Size: int64(len(b))}, nil
}

func (m *inMemoryStorage) Delete(ctx context.Context, key string) error { return nil }

func (m *inMemoryStorage) Exists(ctx context.Context, key string) (bool, error) {
	_, ok := m.data[key]
	return ok, nil
}

func (m *inMemoryStorage) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "", nil
}

// treeWalkHandlerRouter wires only the endpoint under test. We
// don't want middleware noise interfering with the assertion path.
func treeWalkHandlerRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Post("/answer/treewalk", d.handleAnswerTreeWalk)
	})
	return r
}

// buildTreeWalkTestTree mirrors the strategy tests' tree so
// assertions about which section IDs surface in citations stay
// consistent across the two suites.
func buildTreeWalkTestTree() *tree.Tree {
	a1 := &tree.Section{ID: "sec_a1", ParentID: "sec_a", Title: "Install", Summary: "install steps", ContentRef: "a1_ref", PageStart: 1, PageEnd: 2}
	a2 := &tree.Section{ID: "sec_a2", ParentID: "sec_a", Title: "Config", Summary: "config keys", ContentRef: "a2_ref", PageStart: 3, PageEnd: 4}
	b1 := &tree.Section{ID: "sec_b1", ParentID: "sec_b", Title: "Querying", Summary: "how to query", ContentRef: "b1_ref", PageStart: 5, PageEnd: 7}
	b2 := &tree.Section{ID: "sec_b2", ParentID: "sec_b", Title: "Debt", Summary: "long-term debt", ContentRef: "b2_ref", PageStart: 8, PageEnd: 9}
	a := &tree.Section{ID: "sec_a", ParentID: "sec_root", Title: "Setup", Summary: "setup", Children: []*tree.Section{a1, a2}, PageStart: 1, PageEnd: 4}
	b := &tree.Section{ID: "sec_b", ParentID: "sec_root", Title: "Usage", Summary: "usage", Children: []*tree.Section{b1, b2}, PageStart: 5, PageEnd: 9}
	root := &tree.Section{ID: "sec_root", Title: "Atlas", Children: []*tree.Section{a, b}, PageStart: 1, PageEnd: 9}
	return &tree.Tree{DocumentID: "doc_x", Title: "Atlas", Root: root}
}

// newTestDeps wires the minimum surface for the treewalk handler
// to run end-to-end against httptest. The strategy is constructed
// directly (no DB / cache wrapper) so per-test LLM scripting
// drives behaviour deterministically.
func newTestDeps(t *testing.T, replies ...string) (Deps, *treeWalkScriptedLLM, *inMemoryStorage) {
	t.Helper()

	llm := &treeWalkScriptedLLM{replies: replies}
	store := &inMemoryStorage{data: map[string][]byte{
		"a1_ref": []byte("Install steps: run vle ingest..."),
		"a2_ref": []byte("Config keys: VLE_FOO, VLE_BAR."),
		"b1_ref": []byte("How to query the API."),
		"b2_ref": []byte("Debt registration is in line items A and B."),
	}}
	strat := retrieval.NewTreeWalkStrategy(llm)
	strat.PageLoader = pageStorageLoader{s: store}

	deps := Deps{
		Logger:            slog.Default(),
		Storage:           store,
		LLM:               llm,
		LLMModel:          "test-model",
		Strategy:          strat, // unrelated to /v1/answer/treewalk; populated for sanity
		TreeWalkStrategy: strat,
		TreeWalk:         config.TreeWalkBlock{Enabled: true, MaxHops: 8, PageContentLimit: 16000},
		AnswerSpan:        config.AnswerSpanBlock{Enabled: false},
		Replay: retrieval.NewLRUReplayStore(retrieval.LRUReplayConfig{
			MaxEntries: 16,
			TTL:        5 * time.Minute,
		}),
		TreeWalkTreeLoader: func(ctx context.Context, docID tree.DocumentID) (*tree.Tree, error) {
			if docID != "doc_x" {
				return nil, fmt.Errorf("unknown document %q (test loader only knows doc_x)", docID)
			}
			return buildTreeWalkTestTree(), nil
		},
	}
	return deps, llm, store
}

// pageStorageLoader adapts the in-memory storage to the
// PageContentLoader interface the strategy expects. The
// production engine uses an identical adapter in cmd/engine/main.go;
// duplicating it here keeps the test self-contained.
type pageStorageLoader struct{ s storage.Storage }

func (l pageStorageLoader) Load(ctx context.Context, ref string) ([]byte, error) {
	rc, _, err := l.s.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// TestHandleAnswerTreeWalkHappyPath: the canonical 3-tool
// sequence ends with a JSON response carrying answer, citations,
// hops_taken, trace_token, pages_read, and a usage block. The
// LLM is NOT called for span extraction in this test path because
// AnswerSpan.Enabled is false at the config-block level — but the
// citations still surface section_ids and page ranges.
func TestHandleAnswerTreeWalkHappyPath(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t,
		`{"tool":"get_document_structure","reasoning":"orient"}`,
		`{"tool":"get_pages","start_page":1,"end_page":2,"reasoning":"install lives here"}`,
		`{"tool":"done","answer":"Run vle ingest.","cited_pages":[[1,2]],"reasoning":"install on pages 1-2"}`,
	)

	body := strings.NewReader(`{"document_id":"doc_x","query":"how do I install?","model":"test-model"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, rec.Body.String())
	}
	if resp["answer"].(string) != "Run vle ingest." {
		t.Errorf("answer = %v, want \"Run vle ingest.\"", resp["answer"])
	}
	if resp["strategy"].(string) != "treewalk" {
		t.Errorf("strategy = %v, want treewalk", resp["strategy"])
	}
	if resp["hops_taken"].(float64) != 3 {
		t.Errorf("hops_taken = %v, want 3", resp["hops_taken"])
	}
	if resp["trace_token"].(string) == "" {
		t.Error("trace_token must be non-empty on success")
	}
	cits, ok := resp["citations"].([]any)
	if !ok || len(cits) == 0 {
		t.Fatalf("citations missing or empty: %v", resp["citations"])
	}
	first := cits[0].(map[string]any)
	if first["start_page"].(float64) != 1 || first["end_page"].(float64) != 2 {
		t.Errorf("first citation page range = %v-%v, want 1-2", first["start_page"], first["end_page"])
	}
	secs, ok := first["section_ids"].([]any)
	if !ok || len(secs) == 0 {
		t.Errorf("first citation must list section_ids, got %v", first["section_ids"])
	}
	// pages_read must surface the get_pages invocation
	pages, ok := resp["pages_read"].([]any)
	if !ok || len(pages) != 1 {
		t.Errorf("pages_read = %v, want 1 entry", resp["pages_read"])
	}
}

// TestHandleAnswerTreeWalkReasoningTrace: with reasoning=true,
// the response carries a reasoning_trace array describing each
// tool call. Each entry must have hop + tool + (optional) args.
func TestHandleAnswerTreeWalkReasoningTrace(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t,
		`{"tool":"get_document_structure","reasoning":"orient"}`,
		`{"tool":"get_pages","start_page":3,"end_page":4,"reasoning":"look at config"}`,
		`{"tool":"done","answer":"Config keys are VLE_*","cited_pages":[[3,4]],"reasoning":"config on 3-4"}`,
	)

	body := strings.NewReader(`{"document_id":"doc_x","query":"config?","reasoning":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	trace, ok := resp["reasoning_trace"].([]any)
	if !ok || len(trace) != 3 {
		t.Fatalf("reasoning_trace = %v, want 3 entries", resp["reasoning_trace"])
	}
	// Every entry must have a hop number and a tool tag.
	for i, raw := range trace {
		entry := raw.(map[string]any)
		if _, ok := entry["hop"]; !ok {
			t.Errorf("trace entry %d missing hop", i)
		}
		if _, ok := entry["tool"]; !ok {
			t.Errorf("trace entry %d missing tool", i)
		}
	}
	// The get_pages entry must surface its args.
	if args, ok := trace[1].(map[string]any)["args"].(map[string]any); !ok {
		t.Errorf("get_pages trace entry missing args, got %v", trace[1])
	} else {
		if args["start_page"].(float64) != 3 || args["end_page"].(float64) != 4 {
			t.Errorf("trace args = %v, want start=3 end=4", args)
		}
	}
}

// TestHandleAnswerTreeWalkDedupAndCapCitations is the end-to-end
// proof of the bench-facing fix: a done that sprays the SAME range
// five times plus extras must produce a citations[] array that is
// deduped and capped at MaxCitations — no duplicate page ranges, no
// repeated section ids across citations, and confidence surfaced.
// This is the API-layer mirror of the strategy's dedup test and the
// reason precision@5 stops deflating.
func TestHandleAnswerTreeWalkDedupAndCapCitations(t *testing.T) {
	t.Parallel()

	// Read one range, then a done that cites [1,2] five times plus
	// two more distinct ranges and a confidence. MaxCitations=3.
	deps, _, _ := newTestDeps(t,
		`{"tool":"get_pages","start_page":1,"end_page":2,"reasoning":"skim"}`,
		`{"tool":"done","answer":"sprayed","confidence":0.8,"cited_pages":[[1,2],[1,2],[1,2],[1,2],[1,2],[3,4],[8,9]]}`,
	)

	body := strings.NewReader(`{"document_id":"doc_x","query":"q"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cits, ok := resp["citations"].([]any)
	if !ok {
		t.Fatalf("citations missing: %v", resp["citations"])
	}
	// Capped at MaxCitations=3, and every page range distinct.
	if len(cits) > 3 {
		t.Errorf("citations must be capped at 3, got %d", len(cits))
	}
	seenRange := map[[2]int]int{}
	for _, raw := range cits {
		c := raw.(map[string]any)
		key := [2]int{int(c["start_page"].(float64)), int(c["end_page"].(float64))}
		seenRange[key]++
	}
	for key, n := range seenRange {
		if n > 1 {
			t.Errorf("citation page range %v appears %d times; must be deduped", key, n)
		}
	}
	// The three distinct ranges all survive (under the cap).
	for _, want := range [][2]int{{1, 2}, {3, 4}, {8, 9}} {
		if seenRange[want] == 0 {
			t.Errorf("expected a citation for range %v, citations: %v", want, cits)
		}
	}
	// confidence surfaces on the response.
	if conf, ok := resp["confidence"].(float64); !ok || conf != 0.8 {
		t.Errorf("response confidence = %v, want 0.8", resp["confidence"])
	}
}

// TestHandleAnswerTreeWalkConfidentSingleCitation is the happy half
// at the API layer: a confident single-range done — even after the
// model skimmed several pages — surfaces exactly ONE citation. This
// is the f1=1.0 commit case, and the fix that stops a multi-page
// navigation footprint from leaking into citations[].
func TestHandleAnswerTreeWalkConfidentSingleCitation(t *testing.T) {
	t.Parallel()

	// The model reads pages 1-2 AND 8-9 while searching, but commits
	// to a single cited range [8,9]. citations[] must be just [8,9].
	deps, _, _ := newTestDeps(t,
		`{"tool":"get_pages","start_page":1,"end_page":2,"reasoning":"check setup"}`,
		`{"tool":"get_pages","start_page":8,"end_page":9,"reasoning":"check debt"}`,
		`{"tool":"done","answer":"Debt is on 8-9.","confidence":0.95,"cited_pages":[[8,9]],"reasoning":"clear"}`,
	)

	body := strings.NewReader(`{"document_id":"doc_x","query":"debt?"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	cits, ok := resp["citations"].([]any)
	if !ok || len(cits) != 1 {
		t.Fatalf("confident single pick must yield exactly ONE citation, got %v", resp["citations"])
	}
	first := cits[0].(map[string]any)
	if first["start_page"].(float64) != 8 || first["end_page"].(float64) != 9 {
		t.Errorf("citation = %v-%v, want 8-9", first["start_page"], first["end_page"])
	}
	// pages_read still records the full navigation footprint (both
	// reads) — only citations[] is tightened to the commitment.
	if pages, ok := resp["pages_read"].([]any); !ok || len(pages) != 2 {
		t.Errorf("pages_read must keep the full footprint (2 reads), got %v", resp["pages_read"])
	}
}

// TestHandleAnswerTreeWalkReasoningTraceQueryParam: the
// ?reasoning=true query param is an alternative to the body field.
// Some clients prefer it for GET-friendliness when prototyping.
func TestHandleAnswerTreeWalkReasoningTraceQueryParam(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t,
		`{"tool":"done","answer":"x","cited_pages":[]}`,
	)

	body := strings.NewReader(`{"document_id":"doc_x","query":"q"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk?reasoning=true", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["reasoning_trace"]; !ok {
		t.Errorf("?reasoning=true must produce reasoning_trace, got: %v", resp)
	}
}

// TestHandleAnswerTreeWalkBadRequest: missing document_id /
// query → 400.
func TestHandleAnswerTreeWalkBadRequest(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t)

	for _, body := range []string{
		`{}`,
		`{"document_id":"doc_x"}`, // missing query
		`{"query":"q"}`,           // missing document_id
		`not-json`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", strings.NewReader(body))
		rec := httptest.NewRecorder()
		treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestHandleAnswerTreeWalkDocumentNotFound: a tree-loader that
// returns ErrNotFound bubbles up as 404. The test loader rejects
// unknown doc IDs.
func TestHandleAnswerTreeWalkDocumentNotFound(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t)
	// Re-wire the loader to return ErrNotFound for the right error
	// path. The default test loader returns a generic error
	// (different status — also valid but less specific).
	deps.TreeWalkTreeLoader = func(ctx context.Context, docID tree.DocumentID) (*tree.Tree, error) {
		return nil, dbNotFoundError()
	}

	body := strings.NewReader(`{"document_id":"missing","query":"q"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestHandleAnswerTreeWalkDisabled: when TreeWalk.Enabled=false
// or TreeWalkStrategy is nil, the endpoint returns 501. Two
// failure modes, both must produce the same status.
func TestHandleAnswerTreeWalkDisabled(t *testing.T) {
	t.Parallel()

	// Mode 1: config disabled.
	deps, _, _ := newTestDeps(t)
	deps.TreeWalk.Enabled = false

	body := strings.NewReader(`{"document_id":"doc_x","query":"q"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("config disabled: status = %d, want 501", rec.Code)
	}

	// Mode 2: strategy nil.
	deps2, _, _ := newTestDeps(t)
	deps2.TreeWalkStrategy = nil

	body = strings.NewReader(`{"document_id":"doc_x","query":"q"}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec = httptest.NewRecorder()
	treeWalkHandlerRouter(deps2).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("strategy nil: status = %d, want 501", rec.Code)
	}
}

// TestHandleAnswerTreeWalkNoLLM: no LLM client → 501.
func TestHandleAnswerTreeWalkNoLLM(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t)
	deps.LLM = nil

	body := strings.NewReader(`{"document_id":"doc_x","query":"q"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

// TestHandleAnswerTreeWalkReplayPersisted: the response is
// stored in the replay store under its trace_token, and the
// existing /v1/replay handler returns the byte-identical body.
func TestHandleAnswerTreeWalkReplayPersisted(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t,
		`{"tool":"done","answer":"X","cited_pages":[[5,7]]}`,
	)

	body := strings.NewReader(`{"document_id":"doc_x","query":"replay-me"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first call: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	originalBody := rec.Body.Bytes()

	var resp map[string]any
	_ = json.Unmarshal(originalBody, &resp)
	token := resp["trace_token"].(string)
	if token == "" {
		t.Fatal("trace_token must be populated for replay")
	}

	// Now hit /v1/replay with the same token + query + doc id.
	r2 := chi.NewRouter()
	r2.Route("/v1", func(r chi.Router) {
		r.Post("/replay", deps.handleReplay)
	})
	replayBody := strings.NewReader(fmt.Sprintf(`{"trace_token":%q,"query":"replay-me","document_id":"doc_x"}`, token))
	req2 := httptest.NewRequest(http.MethodPost, "/v1/replay", replayBody)
	rec2 := httptest.NewRecorder()
	r2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	// The original /v1/answer/treewalk response carries a trailing
	// newline from marshalJSONForReplay; the replay path returns
	// the exact stored bytes, so we compare with the newline.
	if !bytes.Equal(originalBody, rec2.Body.Bytes()) {
		t.Errorf("replay bytes differ from original\nORIG: %s\nREP : %s", originalBody, rec2.Body.Bytes())
	}
}

// TestHandleAnswerTreeWalkStreaming: with stream=true, the
// response is SSE with one event per tool call plus a started +
// answer event. The data payloads are JSON.
func TestHandleAnswerTreeWalkStreaming(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t,
		`{"tool":"get_document_structure"}`,
		`{"tool":"get_pages","start_page":1,"end_page":2}`,
		`{"tool":"done","answer":"streamed","cited_pages":[[1,2]]}`,
	)

	body := strings.NewReader(`{"document_id":"doc_x","query":"q","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: started",
		"event: get_document_structure",
		"event: get_pages",
		"event: done",
		"event: answer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing SSE %q in stream body:\n%s", want, out)
		}
	}
}

// TestHandleAnswerTreeWalkPerRequestOverrides: max_hops and
// max_pages_per_fetch on the body override the engine's config.
// We can't measure max_pages_per_fetch from outside (it shapes
// content size, not response shape), but we can verify max_hops
// caps the loop. Set max_hops=1 and a script that emits
// 5 turns — the strategy must stop after 1.
func TestHandleAnswerTreeWalkPerRequestOverrides(t *testing.T) {
	t.Parallel()

	// 6 replies but max_hops=1 → only the first runs as a normal
	// hop, then forceDone kicks in (2 LLM calls total counting the
	// force-done turn). The model never emits a valid done, so
	// the response answer is empty.
	deps, _, _ := newTestDeps(t,
		`{"tool":"get_pages","start_page":1,"end_page":2}`,
		`{"tool":"get_pages","start_page":3,"end_page":4}`,
		`{"tool":"get_pages","start_page":5,"end_page":6}`,
		`{"tool":"get_pages","start_page":7,"end_page":9}`,
		`{"tool":"get_pages","start_page":1,"end_page":1}`,
	)

	body := strings.NewReader(`{"document_id":"doc_x","query":"q","max_hops":1}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// hops_taken includes the forced done turn, so cap=1 → at most
	// 2 actual calls.
	if hops, ok := resp["hops_taken"].(float64); !ok || hops > 2 {
		t.Errorf("hops_taken = %v, want <=2 (max_hops=1 + 1 force-done)", resp["hops_taken"])
	}
}

// TestHandleAnswerTreeWalkTOCFallback: with a tree that has
// page metadata but no persisted TOC, the synthesised TOC drives
// the get_document_structure tool. This test runs end-to-end and
// asserts the response shape; the strategy-level test covers the
// synthesis logic directly.
func TestHandleAnswerTreeWalkTOCFallback(t *testing.T) {
	t.Parallel()

	deps, _, _ := newTestDeps(t,
		`{"tool":"get_document_structure"}`,
		`{"tool":"done","answer":"saw the toc","cited_pages":[]}`,
	)
	// TreeWalkStrategy.TOC is left nil — the synthesised path is
	// the default for any deployment without PR-A merged.

	body := strings.NewReader(`{"document_id":"doc_x","query":"what is in the doc?"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/answer/treewalk", body)
	rec := httptest.NewRecorder()
	treeWalkHandlerRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["answer"].(string) != "saw the toc" {
		t.Errorf("answer = %v, want 'saw the toc'", resp["answer"])
	}
}

// dbNotFoundError returns the real db.ErrNotFound sentinel so the
// handler's errors.Is(err, db.ErrNotFound) check fires.
func dbNotFoundError() error {
	return db.ErrNotFound
}
