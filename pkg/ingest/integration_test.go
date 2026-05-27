package ingest

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TestPipelineRunParallelSummarizeAndHyDEIntegration is the
// production-shaped sanity check for task #24: ingest a small fixture
// end-to-end via Pipeline.Run, then assert every persisted section ends
// up with a populated summary AND every leaf section has
// candidate_questions populated. The fact that both columns are filled
// without us calling p.fail proves the parallel summarize/HyDE
// orchestration introduced in this PR works under real DB / storage
// I/O.
//
// Gated on TEST_DATABASE_URL so the default `go test ./...` run stays
// fast and DB-free. Run locally with:
//
//	TEST_DATABASE_URL=postgres://vle:vle@localhost:5432/vle_test?sslmode=disable \
//	    go test -run TestPipelineRunParallelSummarizeAndHyDEIntegration ./pkg/ingest/
func TestPipelineRunParallelSummarizeAndHyDEIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping ingest pipeline integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pool.Close()
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	tmpDir := t.TempDir()
	store, err := storage.NewLocal(tmpDir)
	if err != nil {
		t.Fatalf("init local storage: %v", err)
	}

	// Load the markdown fixture and stage its bytes in storage at the
	// canonical SourceKey, mirroring what an upload would do.
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

	const (
		orgID   = "org_test_24"
		storeID = db.NilScope
	)
	if err := pool.NewDocument(ctx, db.Document{
		ID:          docID,
		OrgID:       orgID,
		StoreID:     storeID,
		Title:       "rust-ownership.md",
		ContentType: "text/markdown",
		SourceRef:   srcKey,
		ByteSize:    int64(len(fixture)),
	}); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	// Best-effort cleanup so reruns stay isolated.
	t.Cleanup(func() {
		_ = pool.DeleteDocument(context.Background(), docID, orgID, storeID)
	})

	// Stub LLM: returns a deterministic single-sentence summary OR a
	// 3-question JSON depending on which prompt the call carries. This
	// is enough to drive both stages through to a populated DB row.
	// Also records the order in which summarize vs HyDE calls land —
	// we assert at least one HyDE call started before the very last
	// summarize call to prove the stages interleaved in the real
	// pipeline, not just in the unit test.
	var (
		summCalls, hydeCalls atomic.Int32
		// Times of the first HyDE call and the last summarize call,
		// captured to verify interleave without a strict ordering
		// requirement (which would be flaky under varying scheduler load).
		firstHyDETime, lastSummarizeTime atomic.Int64
	)
	llm := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			userMsg := ""
			for _, m := range req.Messages {
				if m.Role == llmgate.RoleUser {
					userMsg = m.Content
				}
			}
			now := time.Now().UnixNano()
			if strings.Contains(userMsg, "Produce up to") {
				// HyDE prompt — return JSON.
				if hydeCalls.Add(1) == 1 {
					firstHyDETime.Store(now)
				}
				return &llmgate.Response{Content: `{"questions":["What is ownership?","How does the stack differ from the heap?","Why does Rust track ownership at compile time?"]}`}, nil
			}
			// Summarize prompt — return a short sentence. Tiny sleep so
			// the summarize stage actually takes nonzero wall time and
			// the HyDE stage has a chance to overlap.
			summCalls.Add(1)
			time.Sleep(15 * time.Millisecond)
			lastSummarizeTime.Store(time.Now().UnixNano())
			return &llmgate.Response{Content: "A concise sentence describing the section's concrete content."}, nil
		},
	}

	pipeline := NewPipeline(Pipeline{
		DB:                   pool,
		Storage:              store,
		LLM:                  llm,
		Parsers:              DefaultRegistry(),
		Logger:               slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		HyDEEnabled:          true,
		HyDENumQuestions:     3,
		HyDEConcurrency:      4,
		SummaryConcurrency:   4,
		GlobalLLMConcurrency: 6,
	})

	if err := pipeline.Run(ctx, Payload{
		DocumentID:  docID,
		ContentType: "text/markdown",
		Filename:    "rust-ownership.md",
		SourceRef:   srcKey,
	}); err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}

	// Assert document moved to ready (no p.fail was called).
	doc, err := pool.GetDocument(ctx, docID, orgID, storeID)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if doc.Status != db.StatusReady {
		t.Fatalf("doc status = %s (err=%q); pipeline did not complete successfully", doc.Status, doc.ErrorMessage)
	}

	sections, err := pool.ListSectionsForWorker(ctx, docID)
	if err != nil {
		t.Fatalf("ListSectionsForWorker: %v", err)
	}
	if len(sections) == 0 {
		t.Fatal("parser produced zero sections from the fixture")
	}

	hasChildren := map[tree.SectionID]bool{}
	for _, s := range sections {
		if s.ParentID != "" {
			hasChildren[s.ParentID] = true
		}
	}

	var missingSummary, missingQuestions []tree.SectionID
	for _, s := range sections {
		if strings.TrimSpace(s.Summary) == "" {
			missingSummary = append(missingSummary, s.ID)
		}
		// HyDE only targets leaves (internal nodes are skipped on purpose).
		if !hasChildren[s.ID] && len(s.CandidateQuestions) == 0 {
			missingQuestions = append(missingQuestions, s.ID)
		}
	}
	if len(missingSummary) > 0 {
		t.Errorf("%d/%d sections missing summary: %v", len(missingSummary), len(sections), missingSummary)
	}
	if len(missingQuestions) > 0 {
		t.Errorf("%d leaf sections missing candidate_questions: %v", len(missingQuestions), missingQuestions)
	}

	if got := hydeCalls.Load(); got == 0 {
		t.Error("no HyDE calls observed — stage did not run")
	}
	if got := summCalls.Load(); got == 0 {
		t.Error("no summarize calls observed — stage did not run")
	}

	// Interleave evidence: the first HyDE call's timestamp must precede
	// the last summarize call's timestamp. If summarize had to finish
	// before HyDE started (the old sequential pipeline), the first HyDE
	// call would land AFTER the last summarize call.
	firstHyDE := firstHyDETime.Load()
	lastSum := lastSummarizeTime.Load()
	if firstHyDE == 0 || lastSum == 0 {
		t.Fatalf("missing timing samples: firstHyDE=%d lastSummarize=%d", firstHyDE, lastSum)
	}
	if firstHyDE >= lastSum {
		t.Errorf("stages did not interleave: first HyDE @ %d, last summarize @ %d", firstHyDE, lastSum)
	}

	t.Logf("sections=%d summarize_calls=%d hyde_calls=%d", len(sections), summCalls.Load(), hydeCalls.Load())
}
