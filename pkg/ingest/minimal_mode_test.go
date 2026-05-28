package ingest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// fakeDocStore is an in-memory docPersister. It captures the status
// transitions and section upserts the minimal pipeline performs so the
// "reaches ready, sections persisted" guarantee can be asserted without
// a live Postgres. Safe for the pipeline's concurrent use (minimal mode
// is sequential, but the mutex keeps the race detector quiet regardless).
type fakeDocStore struct {
	mu       sync.Mutex
	status   db.DocumentStatus
	errMsg   string
	title    string
	sections []db.Section
}

func (f *fakeDocStore) SetDocumentStatus(_ context.Context, _ tree.DocumentID, s db.DocumentStatus, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
	f.errMsg = errMsg
	return nil
}

func (f *fakeDocStore) SetDocumentTitle(_ context.Context, _ tree.DocumentID, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.title = title
	return nil
}

func (f *fakeDocStore) UpsertSection(_ context.Context, s db.Section) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sections = append(f.sections, s)
	return nil
}

func (f *fakeDocStore) snapshot() (db.DocumentStatus, string, []db.Section) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.Section, len(f.sections))
	copy(out, f.sections)
	return f.status, f.errMsg, out
}

// failIfCalledLLM is an llmgate.Client that fails the test the instant
// any LLM call is issued. It is the proof harness for minimal mode:
// minimal ingest must do ZERO LLM work, so a single Complete call is a
// hard test failure. Calls() lets the test assert the counter stayed 0.
type failIfCalledLLM struct {
	t     *testing.T
	calls int
	mu    sync.Mutex
}

func (l *failIfCalledLLM) Complete(_ context.Context, _ llmgate.Request) (*llmgate.Response, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	l.t.Helper()
	l.t.Errorf("minimal mode issued an LLM Complete call; it must do zero LLM work")
	return nil, llmgate.ErrNotImplemented
}

func (l *failIfCalledLLM) CountTokens(_ context.Context, text string) (int, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	l.t.Helper()
	l.t.Errorf("minimal mode issued an LLM CountTokens call; it must do zero LLM work")
	return len(text) / 4, nil
}

func (l *failIfCalledLLM) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestMinimalModeZeroLLMCalls is the headline guarantee: a minimal-mode
// pipeline run reaches StatusReady with sections persisted while making
// ZERO LLM calls. The LLM client fails the test on any call, and we also
// assert its call counter stayed at 0 — together proving minimal ingest
// is pure-Go (parse → persist → ready), no summarize / HyDE / multi-axis
// / TOC.
func TestMinimalModeZeroLLMCalls(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("init local storage: %v", err)
	}

	fixture, err := os.ReadFile("../../testdata/rust-ownership.md")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	docID := NewDocumentID()
	srcKey := SourceKey(docID, "rust-ownership.md")
	if err := store.Put(ctx, srcKey, bytes.NewReader(fixture), storage.Metadata{
		ContentType: "text/markdown",
		Size:        int64(len(fixture)),
	}); err != nil {
		t.Fatalf("stage source: %v", err)
	}

	llm := &failIfCalledLLM{t: t}

	// Construct the pipeline through NewPipeline (the production path) in
	// minimal mode. HyDE/SummaryAxes flags are intentionally left at
	// their full-mode-on values to prove the minimal switch — not a pile
	// of disabled sub-flags — is what suppresses the LLM work.
	p := NewPipeline(Pipeline{
		DB:                 nil, // never touched: runMinimal takes the store explicitly
		Storage:            store,
		LLM:                llm,
		Parsers:            DefaultRegistry(),
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Mode:               ModeMinimal,
		HyDEEnabled:        true,
		SummaryAxesEnabled: true,
		TOCEnabled:         true,
	})

	fake := &fakeDocStore{}
	if err := p.runMinimal(ctx, fake, Payload{
		DocumentID:  docID,
		ContentType: "text/markdown",
		Filename:    "rust-ownership.md",
		SourceRef:   srcKey,
	}); err != nil {
		t.Fatalf("runMinimal: %v", err)
	}

	status, errMsg, sections := fake.snapshot()
	if status != db.StatusReady {
		t.Fatalf("doc status = %q (err=%q); minimal mode did not reach ready", status, errMsg)
	}
	if len(sections) == 0 {
		t.Fatal("minimal mode persisted zero sections")
	}
	if n := llm.callCount(); n != 0 {
		t.Fatalf("minimal mode made %d LLM calls; want 0", n)
	}

	// No summaries / axes / candidate-questions were written — minimal
	// mode skips every enrichment stage, so every persisted section is
	// bare (title + content ref only).
	for _, s := range sections {
		if s.Summary != "" {
			t.Errorf("section %s carries a summary in minimal mode: %q", s.ID, s.Summary)
		}
		if s.SummaryAxes != nil {
			t.Errorf("section %s carries summary_axes in minimal mode", s.ID)
		}
		if len(s.CandidateQuestions) != 0 {
			t.Errorf("section %s carries HyDE questions in minimal mode", s.ID)
		}
	}
}

// TestMinimalModeReadyIsQueryable proves a minimal-ingested document is
// usable by the page-based retrieval strategy's two run-time inputs:
//
//  1. the synthesised TOC (documents.toc_tree is NULL after minimal
//     ingest, so the strategy falls back to synthesiseTOC over the
//     section tree) — must be a non-empty, title-bearing structure; and
//  2. raw section bodies read from storage via the section ContentRef —
//     must return the persisted text.
//
// It reconstructs the tree from exactly what runMinimal persisted, so it
// exercises the real post-ingest shape. The end-to-end PageIndexStrategy
// loop is covered in pkg/retrieval (TestPageIndexMinimalIngestedDoc).
func TestMinimalModeReadyIsQueryable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("init local storage: %v", err)
	}

	fixture, err := os.ReadFile("../../testdata/rust-ownership.md")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	docID := NewDocumentID()
	srcKey := SourceKey(docID, "rust-ownership.md")
	if err := store.Put(ctx, srcKey, bytes.NewReader(fixture), storage.Metadata{
		ContentType: "text/markdown",
		Size:        int64(len(fixture)),
	}); err != nil {
		t.Fatalf("stage source: %v", err)
	}

	p := NewPipeline(Pipeline{
		Storage: store,
		LLM:     &failIfCalledLLM{t: t},
		Parsers: DefaultRegistry(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Mode:    ModeMinimal,
	})
	fake := &fakeDocStore{}
	if err := p.runMinimal(ctx, fake, Payload{
		DocumentID:  docID,
		ContentType: "text/markdown",
		Filename:    "rust-ownership.md",
		SourceRef:   srcKey,
	}); err != nil {
		t.Fatalf("runMinimal: %v", err)
	}
	_, _, sections := fake.snapshot()

	// Reconstruct the tree from persisted rows (mirrors db.buildTree's
	// parent→children wiring) and confirm it is non-trivial.
	root := reconstructTree(docID, fake.title, sections)
	if root == nil {
		t.Fatal("reconstructed tree root is nil; minimal mode persisted nothing usable")
	}

	// (1) Synthesised TOC fallback returns a usable, title-bearing view.
	titleSeen := false
	var walk func(*tree.Section)
	walk = func(s *tree.Section) {
		if s == nil {
			return
		}
		if s.Title != "" {
			titleSeen = true
		}
		for _, c := range s.Children {
			walk(c)
		}
	}
	walk(root)
	if !titleSeen {
		t.Error("reconstructed section tree carries no titles; synthesised TOC would be empty")
	}

	// (2) At least one persisted leaf has a ContentRef whose bytes load
	// back from storage — the raw text the page strategy reads at query
	// time.
	loadedSomeBody := false
	for _, s := range sections {
		if s.ContentRef == "" {
			continue
		}
		rc, _, err := store.Get(ctx, s.ContentRef)
		if err != nil {
			t.Fatalf("load section %s content: %v", s.ID, err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		if len(bytes.TrimSpace(body)) > 0 {
			loadedSomeBody = true
		}
	}
	if !loadedSomeBody {
		t.Error("no section body loaded from storage; page strategy would have no raw text to read")
	}
}

// reconstructTree wires a flat db.Section list into a tree.Section root,
// matching db.buildTree's behaviour (which is unexported): a single
// top-level section becomes the root; multiple are wrapped in a
// synthetic empty-ID root carrying the document title.
func reconstructTree(_ tree.DocumentID, title string, rows []db.Section) *tree.Section {
	byID := make(map[tree.SectionID]*tree.Section, len(rows))
	for _, r := range rows {
		byID[r.ID] = &tree.Section{
			ID:         r.ID,
			ParentID:   r.ParentID,
			Ordinal:    r.Ordinal,
			Title:      r.Title,
			ContentRef: r.ContentRef,
			PageStart:  r.PageStart,
			PageEnd:    r.PageEnd,
		}
	}
	var topLevel []*tree.Section
	for _, r := range rows {
		s := byID[r.ID]
		if s.ParentID == "" {
			topLevel = append(topLevel, s)
			continue
		}
		if parent, ok := byID[s.ParentID]; ok {
			parent.Children = append(parent.Children, s)
		}
	}
	switch len(topLevel) {
	case 0:
		return nil
	case 1:
		return topLevel[0]
	default:
		return &tree.Section{Title: title, Children: topLevel}
	}
}
