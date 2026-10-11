package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Stage states, in the order a stage moves through them.
const (
	StagePending = "pending"
	StageRunning = "running"
	StageDone    = "done"
	StageFailed  = "failed"
	StageSkipped = "skipped"
)

// Stage is one step of ingest as the dashboard draws it. Parent names the
// stage a sub-step belongs to ("contents" for "contents.resolve").
type Stage struct {
	Name      string     `json:"name"`
	Label     string     `json:"label"`
	Parent    string     `json:"parent,omitempty"`
	State     string     `json:"state"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

// progressWriter is the persistence a Progress needs. *db.Pool has it; a
// store without it (the minimal-mode test fake) gets a Progress that only
// keeps state in memory.
type progressWriter interface {
	SetDocumentProgress(ctx context.Context, id tree.DocumentID, progressJSON []byte) error
}

// Progress records the stages of one document's ingest and writes the
// whole list on every change. One ingest job owns it, but the after-ready
// work runs on its own goroutine, so it is safe for concurrent use.
//
// Writes are best effort: a failed progress write is logged and ingest
// carries on, because progress is a view of the work, not the work.
type Progress struct {
	mu     sync.Mutex
	doc    tree.DocumentID
	stages []Stage
	w      progressWriter
	log    *slog.Logger
}

// NewProgress starts a stage list for doc, with every planned stage
// pending, so a reader can draw the whole pipeline from the first write.
func NewProgress(ctx context.Context, w progressWriter, doc tree.DocumentID, log *slog.Logger, plan []Stage) *Progress {
	pr := &Progress{doc: doc, w: w, log: log}
	for _, s := range plan {
		s.State = StagePending
		pr.stages = append(pr.stages, s)
	}
	pr.flush(ctx)
	return pr
}

// Start marks a stage running.
func (pr *Progress) Start(ctx context.Context, name string) {
	pr.update(ctx, name, func(s *Stage, now time.Time) {
		s.State, s.StartedAt, s.EndedAt = StageRunning, &now, nil
	})
}

// Done marks a stage finished, with an optional one-line detail.
func (pr *Progress) Done(ctx context.Context, name, detail string) {
	pr.finish(ctx, name, StageDone, detail)
}

// Fail marks a stage failed with the reason.
func (pr *Progress) Fail(ctx context.Context, name string, cause error) {
	pr.finish(ctx, name, StageFailed, cause.Error())
}

// Skip marks a stage that will not run, and why.
func (pr *Progress) Skip(ctx context.Context, name, why string) {
	pr.finish(ctx, name, StageSkipped, why)
}

// Span records a stage that already happened, from..to: the time a job
// waited in the queue, which ingest only learns once it starts.
func (pr *Progress) Span(ctx context.Context, name string, from, to time.Time, detail string) {
	pr.update(ctx, name, func(s *Stage, _ time.Time) {
		f, t := from.UTC(), to.UTC()
		s.State, s.StartedAt, s.EndedAt, s.Detail = StageDone, &f, &t, detail
	})
}

// Stages returns a copy of the current list.
func (pr *Progress) Stages() []Stage {
	if pr == nil {
		return nil
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return append([]Stage(nil), pr.stages...)
}

func (pr *Progress) finish(ctx context.Context, name, state, detail string) {
	pr.update(ctx, name, func(s *Stage, now time.Time) {
		if s.StartedAt == nil && state != StageSkipped {
			s.StartedAt = &now
		}
		s.State, s.EndedAt, s.Detail = state, &now, detail
	})
}

// update changes one stage (adding it when it was not planned) and writes
// the list. A nil Progress is a no-op, so callers never check.
func (pr *Progress) update(ctx context.Context, name string, f func(*Stage, time.Time)) {
	if pr == nil {
		return
	}
	pr.mu.Lock()
	now := time.Now().UTC()
	i := -1
	for j := range pr.stages {
		if pr.stages[j].Name == name {
			i = j
			break
		}
	}
	if i < 0 {
		pr.stages = append(pr.stages, Stage{Name: name, Label: name, State: StagePending})
		i = len(pr.stages) - 1
	}
	f(&pr.stages[i], now)
	pr.mu.Unlock()
	pr.flush(ctx)
}

func (pr *Progress) flush(ctx context.Context) {
	if pr.w == nil {
		return
	}
	pr.mu.Lock()
	raw, err := json.Marshal(pr.stages)
	pr.mu.Unlock()
	if err != nil {
		return
	}
	// The job's context may already be cancelled when a stage fails;
	// the record of that failure must still land.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := pr.w.SetDocumentProgress(wctx, pr.doc, raw); err != nil && pr.log != nil {
		pr.log.Warn("ingest: progress not recorded", "document_id", string(pr.doc), "err", err)
	}
}

type progressKey struct{}

// withProgress carries a Progress through the run, so the stages deep in
// the pipeline report without every signature taking one.
func withProgress(ctx context.Context, pr *Progress) context.Context {
	return context.WithValue(ctx, progressKey{}, pr)
}

// progressFrom returns the run's Progress, or nil (whose methods no-op).
func progressFrom(ctx context.Context) *Progress {
	pr, _ := ctx.Value(progressKey{}).(*Progress)
	return pr
}

// Stage names. The dashboard keys its pipeline view on them.
const (
	stageQueued   = "queued"
	stageParse    = "parse"
	stageSave     = "save"
	stageSummary  = "summaries"
	stageContents = "contents"
	stagePages    = "pages"
)

// planFor is the stage list a document will go through, in order.
func (p *Pipeline) planFor(pl Payload) []Stage {
	plan := []Stage{
		{Name: stageQueued, Label: "Queued"},
		{Name: stageParse, Label: "Read the file"},
		{Name: stageSave, Label: "Save sections"},
	}
	mode := p.modeFor(pl)
	isPDF := pl.ContentType == "application/pdf"
	if mode != ModeMinimal && mode != ModeTOC {
		plan = append(plan, Stage{Name: stageSummary, Label: "Summarise sections"})
	}
	if isPDF && (mode == ModeTOC || (mode != ModeMinimal && p.TOCEnabled)) {
		plan = append(plan,
			Stage{Name: stageContents, Label: "Build the contents"},
			Stage{Name: stageContents + ".detect", Label: "Find the contents page", Parent: stageContents},
			Stage{Name: stageContents + ".extract", Label: "Read its entries", Parent: stageContents},
			Stage{Name: stageContents + ".resolve", Label: "Place each on its page", Parent: stageContents},
			Stage{Name: stageContents + ".split", Label: "Split long sections", Parent: stageContents},
		)
	}
	if isPDF && p.AfterReady != nil {
		plan = append(plan, Stage{Name: stagePages, Label: "Render pages for citations"})
	}
	return plan
}
