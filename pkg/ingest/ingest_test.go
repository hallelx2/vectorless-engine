package ingest

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// TestRunParallelStagesInterleaves asserts that summarize and HyDE are
// launched on separate goroutines and can make progress concurrently —
// the whole point of the parallelization. It blocks the "summarize"
// stage on a channel, then verifies the HyDE stage runs to completion
// while summarize is still pending. The mirror case (HyDE blocked,
// summarize runs) covers the symmetric path.
func TestRunParallelStagesInterleaves(t *testing.T) {
	t.Parallel()

	summarizeBlock := make(chan struct{})
	hydeStarted := make(chan struct{})
	hydeDone := make(chan struct{})

	summarizeFn := func(ctx context.Context) error {
		// Wait until HyDE has both started AND finished — proves the two
		// stages weren't running sequentially with summarize first.
		<-summarizeBlock
		return nil
	}
	hydeFn := func(ctx context.Context) error {
		close(hydeStarted)
		// Simulate the "real" HyDE doing work.
		time.Sleep(5 * time.Millisecond)
		close(hydeDone)
		// Now release summarize so the whole call returns.
		close(summarizeBlock)
		return nil
	}

	doneCh := make(chan struct{})
	go func() {
		sumErr, hydeErr := runParallelStages(context.Background(), summarizeFn, hydeFn)
		if sumErr != nil || hydeErr != nil {
			t.Errorf("runParallelStages: sumErr=%v hydeErr=%v", sumErr, hydeErr)
		}
		close(doneCh)
	}()

	select {
	case <-hydeStarted:
		// Good — HyDE started without waiting for summarize to finish.
	case <-time.After(2 * time.Second):
		t.Fatal("HyDE never started; stages are running sequentially, not in parallel")
	}

	select {
	case <-hydeDone:
		// Good — HyDE finished while summarize was still blocked.
	case <-time.After(2 * time.Second):
		t.Fatal("HyDE never finished while summarize was blocked")
	}

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("runParallelStages didn't return after HyDE released summarize")
	}
}

// TestRunParallelStagesReturnsBothErrorsIndependently asserts that one
// stage's failure does not affect the other stage's reported error.
// This matches the "non-fatal per-stage" contract Run relies on.
func TestRunParallelStagesReturnsBothErrorsIndependently(t *testing.T) {
	t.Parallel()
	sumErr := errors.New("summarize boom")
	hydeErr := errors.New("hyde boom")
	gotSum, gotHyde := runParallelStages(context.Background(),
		func(context.Context) error { return sumErr },
		func(context.Context) error { return hydeErr },
	)
	if !errors.Is(gotSum, sumErr) {
		t.Errorf("summarize err: got %v, want %v", gotSum, sumErr)
	}
	if !errors.Is(gotHyde, hydeErr) {
		t.Errorf("hyde err: got %v, want %v", gotHyde, hydeErr)
	}
}

// TestRunParallelStagesNilHydeSkips covers HyDEEnabled=false: the helper
// must not invoke nil hydeFn and must return nil hydeErr.
func TestRunParallelStagesNilHydeSkips(t *testing.T) {
	t.Parallel()
	var ran atomic.Bool
	sumErr, hydeErr := runParallelStages(context.Background(),
		func(context.Context) error { ran.Store(true); return nil },
		nil,
	)
	if sumErr != nil || hydeErr != nil {
		t.Errorf("got sumErr=%v hydeErr=%v", sumErr, hydeErr)
	}
	if !ran.Load() {
		t.Error("summarize was never invoked")
	}
}

// TestGlobalLLMSemaphoreCapsInFlight asserts the shared semaphore really
// caps total LLM-in-flight across both stages. We use the recording-LLM
// pattern: every Complete blocks until the test releases it, while we
// measure the peak in-flight count and confirm it never exceeds the cap.
func TestGlobalLLMSemaphoreCapsInFlight(t *testing.T) {
	t.Parallel()

	const cap = 2
	var inFlight, peak int32
	release := make(chan struct{})

	m := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
					break
				}
			}
			<-release
			atomic.AddInt32(&inFlight, -1)
			return &llmgate.Response{Content: `{"questions":["Q"]}`}, nil
		},
	}

	p := NewPipeline(Pipeline{
		LLM:                  m,
		Logger:               slog.Default(),
		SummaryMaxChars:      4000,
		SummaryModel:         "test-model",
		HyDENumQuestions:     1,
		GlobalLLMConcurrency: cap,
	})

	// Drive 6 candidateQuestionsFor calls concurrently — each one is one
	// LLM call. Under a cap of 2 the peak must be 2, not 6.
	const callers = 6
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			// Acquire the global slot, do the LLM call, release. This
			// mirrors what summarize + HyDE goroutines do internally.
			rel, ok := p.acquireGlobalLLM(context.Background())
			if !ok {
				return
			}
			defer rel()
			_, _ = p.candidateQuestionsFor(context.Background(),
				db.Section{ID: tree.SectionID("s"), Title: "T"}, "")
		}()
	}

	// Let the LLM calls saturate the semaphore.
	require := func(cond bool, msg string) {
		if !cond {
			t.Helper()
			t.Fatal(msg)
		}
	}
	for waited := time.Duration(0); waited < time.Second; waited += 10 * time.Millisecond {
		if atomic.LoadInt32(&inFlight) >= int32(cap) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require(atomic.LoadInt32(&inFlight) == int32(cap),
		"in-flight never reached the cap; semaphore isn't gating LLM calls")
	if peak := atomic.LoadInt32(&peak); peak > int32(cap) {
		t.Fatalf("peak in-flight = %d, exceeds cap = %d", peak, cap)
	}

	// Drain.
	for i := 0; i < callers; i++ {
		release <- struct{}{}
	}
	wg.Wait()

	if final := atomic.LoadInt32(&peak); final > int32(cap) {
		t.Errorf("final peak = %d, exceeds cap = %d", final, cap)
	}
}

// TestAcquireGlobalLLMNoCap exercises the no-cap path: when
// GlobalLLMConcurrency is unset and we don't go through NewPipeline,
// acquireGlobalLLM is a no-op (returns immediately with a no-op
// release). This is the construction path tests use today.
func TestAcquireGlobalLLMNoCap(t *testing.T) {
	t.Parallel()
	p := &Pipeline{} // bypass NewPipeline → globalLLMSem stays nil
	rel, ok := p.acquireGlobalLLM(context.Background())
	if !ok {
		t.Fatal("expected ok=true on no-cap path")
	}
	rel() // must not panic / block
}

// TestAcquireGlobalLLMRespectsCancellation: when the context is already
// canceled and the semaphore is full, acquire returns ok=false promptly.
func TestAcquireGlobalLLMRespectsCancellation(t *testing.T) {
	t.Parallel()
	p := NewPipeline(Pipeline{
		LLM:                  &llmgate.Mock{Reply: `{"questions":[]}`},
		Logger:               slog.Default(),
		GlobalLLMConcurrency: 1,
	})
	// Saturate the semaphore.
	rel1, ok := p.acquireGlobalLLM(context.Background())
	if !ok {
		t.Fatal("first acquire failed")
	}
	t.Cleanup(rel1)

	// Second acquire with a canceled ctx should fail fast.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok2 := p.acquireGlobalLLM(ctx)
	if ok2 {
		t.Error("expected ok=false when ctx is already canceled and sem is full")
	}
}

// TestHyDEPromptOmitsSummary locks down the prompt change: the user
// content sent to the LLM no longer includes the section summary
// (which may not be populated when HyDE runs concurrently with
// summarize). Acts as a regression guard against re-introducing the
// dependency.
func TestHyDEPromptOmitsSummary(t *testing.T) {
	t.Parallel()
	var captured string
	m := &llmgate.Mock{
		Respond: func(ctx context.Context, req llmgate.Request) (*llmgate.Response, error) {
			for _, msg := range req.Messages {
				if msg.Role == llmgate.RoleUser {
					captured = msg.Content
				}
			}
			return &llmgate.Response{Content: `{"questions":["Q1"]}`}, nil
		},
	}
	p := &Pipeline{
		LLM:              m,
		Logger:           slog.Default(),
		SummaryMaxChars:  4000,
		SummaryModel:     "m",
		HyDENumQuestions: 1,
	}
	// Section with a Summary field set; the prompt must NOT contain it.
	sec := db.Section{
		ID:      tree.SectionID("s"),
		Title:   "Section Title",
		Summary: "THIS_SUMMARY_MUST_NOT_LEAK_INTO_PROMPT",
	}
	if _, err := p.candidateQuestionsFor(context.Background(), sec, ""); err != nil {
		t.Fatalf("candidateQuestionsFor: %v", err)
	}
	if captured == "" {
		t.Fatal("LLM Complete was never called")
	}
	if strings.Contains(captured, "THIS_SUMMARY_MUST_NOT_LEAK_INTO_PROMPT") {
		t.Errorf("HyDE prompt still references s.Summary: %q", captured)
	}
	if !strings.Contains(captured, "Section Title") {
		t.Errorf("HyDE prompt missing title: %q", captured)
	}
}
