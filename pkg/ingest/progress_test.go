package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

type recordingWriter struct {
	mu     sync.Mutex
	writes [][]Stage
}

func (w *recordingWriter) SetDocumentProgress(_ context.Context, _ tree.DocumentID, raw []byte) error {
	var s []Stage
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	w.mu.Lock()
	w.writes = append(w.writes, s)
	w.mu.Unlock()
	return nil
}

func (w *recordingWriter) last() []Stage {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes[len(w.writes)-1]
}

func byName(ss []Stage, name string) Stage {
	for _, s := range ss {
		if s.Name == name {
			return s
		}
	}
	return Stage{}
}

// The whole plan is written pending first, then every change rewrites
// the list with the stage's state and times.
func TestProgressRecordsEachStage(t *testing.T) {
	ctx := context.Background()
	w := &recordingWriter{}
	pr := NewProgress(ctx, w, "doc_p", nil, []Stage{{Name: "parse", Label: "Read"}, {Name: "save", Label: "Save"}})
	if got := w.last(); len(got) != 2 || got[0].State != StagePending || got[1].State != StagePending {
		t.Fatalf("first write = %+v, want both stages pending", got)
	}
	pr.Start(ctx, "parse")
	if s := byName(w.last(), "parse"); s.State != StageRunning || s.StartedAt == nil || s.EndedAt != nil {
		t.Fatalf("after Start: %+v", s)
	}
	pr.Done(ctx, "parse", "160 pages")
	pr.Fail(ctx, "save", errors.New("disk full"))
	got := w.last()
	if s := byName(got, "parse"); s.State != StageDone || s.EndedAt == nil || s.Detail != "160 pages" {
		t.Fatalf("parse: %+v", s)
	}
	if s := byName(got, "save"); s.State != StageFailed || s.Detail != "disk full" || s.StartedAt == nil {
		t.Fatalf("save: %+v", s)
	}
	from := time.Now().Add(-3 * time.Second)
	pr.Span(ctx, "queued", from, from.Add(2*time.Second), "")
	if s := byName(w.last(), "queued"); s.State != StageDone || s.EndedAt.Sub(*s.StartedAt) != 2*time.Second {
		t.Fatalf("an unplanned span is added with its own times: %+v", s)
	}
	var nilPR *Progress
	nilPR.Start(ctx, "parse") // a run without progress must not panic
}

func TestPlanForTOCModePDF(t *testing.T) {
	p := &Pipeline{Mode: ModeTOC, AfterReady: func(context.Context, Payload) error { return nil }}
	plan := p.planFor(Payload{ContentType: "application/pdf"})
	want := []string{"queued", "parse", "save", "contents", "contents.detect", "contents.extract", "contents.resolve", "contents.split", "pages"}
	if len(plan) != len(want) {
		t.Fatalf("plan = %+v", plan)
	}
	for i, n := range want {
		if plan[i].Name != n {
			t.Fatalf("stage %d = %s, want %s", i, plan[i].Name, n)
		}
	}
	if plan[5].Parent != "contents" {
		t.Fatalf("sub-steps carry their parent: %+v", plan[5])
	}
	if text := (&Pipeline{Mode: ModeTOC}).planFor(Payload{ContentType: "text/markdown"}); len(text) != 3 {
		t.Fatalf("a text file only parses and saves: %+v", text)
	}
}

// A TOC-mode run reports parse and save through the context.
func TestRunMinimalReportsStages(t *testing.T) {
	ctx := context.Background()
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("# Title\n\n## Section A\n\nAlpha.\n\n## Section B\n\nBeta.\n")
	docID := NewDocumentID()
	pl := Payload{DocumentID: docID, ContentType: "text/markdown", Filename: "doc.md", SourceRef: SourceKey(docID, "doc.md")}
	if err := st.Put(ctx, pl.SourceRef, bytes.NewReader(body), storage.Metadata{ContentType: "text/markdown"}); err != nil {
		t.Fatal(err)
	}
	p := NewPipeline(Pipeline{
		Storage: st, LLM: &failIfCalledLLM{t: t}, Parsers: DefaultRegistry(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Mode: ModeTOC,
	})
	w := &recordingWriter{}
	pr := NewProgress(ctx, w, docID, nil, p.planFor(pl))
	if err := p.runMinimal(withProgress(ctx, pr), &fakeDocStore{}, pl); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"parse", "save"} {
		if s := byName(w.last(), n); s.State != StageDone || s.EndedAt == nil {
			t.Fatalf("%s = %+v, want done", n, s)
		}
	}
}
