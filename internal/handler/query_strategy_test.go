package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/retrieval"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// labeledStrategy is a mock retrieval.Strategy that records whether it
// was invoked and reports a caller-supplied Name. The per-request
// strategy-override test uses it to prove WHICH strategy a /v1/query
// request actually routed to.
type labeledStrategy struct {
	name   string
	picks  []tree.SectionID
	called int32
}

func (s *labeledStrategy) Name() string { return s.name }

func (s *labeledStrategy) Select(ctx context.Context, t *tree.Tree, query string, budget retrieval.ContextBudget) ([]tree.SectionID, error) {
	atomic.AddInt32(&s.called, 1)
	return s.picks, nil
}

func (s *labeledStrategy) wasCalled() bool { return atomic.LoadInt32(&s.called) > 0 }

// memStorage is a minimal in-memory storage.Storage; only Get matters
// for the query handler (it loads section content by ContentRef).
type memStorage struct{ data map[string][]byte }

func (m *memStorage) Put(ctx context.Context, key string, r io.Reader, meta storage.Metadata) error {
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

func (m *memStorage) Get(ctx context.Context, key string) (io.ReadCloser, storage.Metadata, error) {
	b, ok := m.data[key]
	if !ok {
		return nil, storage.Metadata{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), storage.Metadata{Key: key, Size: int64(len(b))}, nil
}

func (m *memStorage) Delete(ctx context.Context, key string) error { return nil }
func (m *memStorage) Exists(ctx context.Context, key string) (bool, error) {
	_, ok := m.data[key]
	return ok, nil
}
func (m *memStorage) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "", nil
}

func queryStrategyTestTree() *tree.Tree {
	a := &tree.Section{ID: "sec_a", Title: "A", ContentRef: "a_ref", PageStart: 1, PageEnd: 2}
	b := &tree.Section{ID: "sec_b", Title: "B", ContentRef: "b_ref", PageStart: 3, PageEnd: 4}
	root := &tree.Section{ID: "sec_root", Title: "Doc", Children: []*tree.Section{a, b}}
	return &tree.Tree{DocumentID: "doc_x", Title: "Doc", Root: root}
}

// newQueryStrategyHandler builds a QueryHandler wired with the default
// strategy + an override set, an in-memory tree loader, and storage,
// so HandleQuery runs end-to-end via httptest without a real DB.
func newQueryStrategyHandler(def retrieval.Strategy, set map[string]retrieval.Strategy) *QueryHandler {
	store := &memStorage{data: map[string][]byte{
		"a_ref": []byte("section A content"),
		"b_ref": []byte("section B content"),
	}}
	h := NewQueryHandler(slog.Default(), nil, store, def, set)
	h.treeLoader = func(ctx context.Context, orgID, storeID string, docID tree.DocumentID) (*tree.Tree, error) {
		return queryStrategyTestTree(), nil
	}
	return h
}

func doQuery(t *testing.T, h *QueryHandler, jsonBody string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(jsonBody))
	req.Header.Set("X-Vectorless-Org", "org_test")
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	var resp map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	}
	return rec, resp
}

// TestHandleQueryStrategyOverrideRoutesToTreeWalk is the Task-2
// acceptance gate: a /v1/query body carrying {"strategy":"treewalk"}
// must route to the page-based strategy, NOT the configured default.
func TestHandleQueryStrategyOverrideRoutesToTreeWalk(t *testing.T) {
	t.Parallel()

	def := &labeledStrategy{name: "chunked-tree", picks: []tree.SectionID{"sec_a"}}
	page := &labeledStrategy{name: "treewalk", picks: []tree.SectionID{"sec_b"}}
	set := map[string]retrieval.Strategy{
		"chunked-tree": def,
		"treewalk":    page,
	}
	h := newQueryStrategyHandler(def, set)

	rec, resp := doQuery(t, h, `{"document_id":"doc_x","query":"q","strategy":"treewalk"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if page.wasCalled() == false {
		t.Error("treewalk strategy was NOT called for {\"strategy\":\"treewalk\"}")
	}
	if def.wasCalled() {
		t.Error("default (chunked-tree) strategy was called despite a treewalk override")
	}
	if got := resp["strategy"]; got != "treewalk" {
		t.Errorf("response strategy = %v, want treewalk", got)
	}
	// The treewalk mock picks sec_b; prove the override's result (not
	// the default's sec_a) is what surfaced.
	secs, _ := resp["sections"].([]any)
	if len(secs) != 1 {
		t.Fatalf("sections = %v, want 1 (sec_b from treewalk)", resp["sections"])
	}
	if id := secs[0].(map[string]any)["id"]; id != "sec_b" {
		t.Errorf("section id = %v, want sec_b (treewalk's pick)", id)
	}
}

// TestHandleQueryDefaultStrategyWhenAbsent: no "strategy" field uses
// the configured default and never touches the override set.
func TestHandleQueryDefaultStrategyWhenAbsent(t *testing.T) {
	t.Parallel()

	def := &labeledStrategy{name: "chunked-tree", picks: []tree.SectionID{"sec_a"}}
	page := &labeledStrategy{name: "treewalk", picks: []tree.SectionID{"sec_b"}}
	set := map[string]retrieval.Strategy{"chunked-tree": def, "treewalk": page}
	h := newQueryStrategyHandler(def, set)

	rec, resp := doQuery(t, h, `{"document_id":"doc_x","query":"q"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !def.wasCalled() {
		t.Error("default strategy was NOT called when no override was set")
	}
	if page.wasCalled() {
		t.Error("override strategy was called despite an absent strategy field")
	}
	if got := resp["strategy"]; got != "chunked-tree" {
		t.Errorf("response strategy = %v, want chunked-tree", got)
	}
}

// TestHandleQueryUnknownStrategy: an override naming a strategy not in
// the set returns 400 rather than silently falling back.
func TestHandleQueryUnknownStrategy(t *testing.T) {
	t.Parallel()

	def := &labeledStrategy{name: "chunked-tree", picks: []tree.SectionID{"sec_a"}}
	set := map[string]retrieval.Strategy{"chunked-tree": def}
	h := newQueryStrategyHandler(def, set)

	rec, _ := doQuery(t, h, `{"document_id":"doc_x","query":"q","strategy":"does-not-exist"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown strategy", rec.Code)
	}
	if def.wasCalled() {
		t.Error("default strategy must not run when an unknown override is rejected")
	}
}

// TestHandleQueryOverrideWithNilSet: a handler wired with no override
// set rejects any non-empty strategy field (400) but still serves
// requests that omit it.
func TestHandleQueryOverrideWithNilSet(t *testing.T) {
	t.Parallel()

	def := &labeledStrategy{name: "chunked-tree", picks: []tree.SectionID{"sec_a"}}
	h := newQueryStrategyHandler(def, nil)

	rec, _ := doQuery(t, h, `{"document_id":"doc_x","query":"q","strategy":"treewalk"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("nil set + override: status = %d, want 400", rec.Code)
	}

	rec2, resp2 := doQuery(t, h, `{"document_id":"doc_x","query":"q"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("nil set + no override: status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	if got := resp2["strategy"]; got != "chunked-tree" {
		t.Errorf("response strategy = %v, want chunked-tree", got)
	}
}
