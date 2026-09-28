package ingest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hallelx2/llmgate"
)

func oneQuestion(id string) llmgate.JudgeRequest {
	return llmgate.JudgeRequest{State: map[string]any{id: "x"}, Questions: map[string]llmgate.Question{id: llmgate.Noul{Instructions: "?"}}}
}

// Every request is in flight at once: each waits until all have arrived,
// which a one-at-a-time loop can never satisfy.
func TestSendJudgeBatchesSendsTogetherAndKeepsOrder(t *testing.T) {
	const n = 5
	var arrived sync.WaitGroup
	arrived.Add(n)
	j := &llmgate.MockJudge{Respond: func(ctx context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		arrived.Done()
		done := make(chan struct{})
		go func() { arrived.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			return nil, errors.New("requests were sent one at a time")
		}
		ans := map[string]llmgate.Answer{}
		for id := range req.Questions {
			var i int
			fmt.Sscanf(id, "q%d", &i)
			ans[id] = llmgate.NoulAnswer{Noul: float64(i) / 10}
		}
		return &llmgate.Judgment{Answers: ans}, nil
	}}
	var reqs []llmgate.JudgeRequest
	for i := 0; i < n; i++ {
		reqs = append(reqs, oneQuestion(fmt.Sprintf("q%d", i)))
	}
	res, err := sendJudgeBatches(context.Background(), j, reqs)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res {
		p, err := r.Noul(fmt.Sprintf("q%d", i))
		if err != nil || p != float64(i)/10 {
			t.Errorf("result %d answers the wrong request: %v %v", i, p, err)
		}
	}
}

// The first failure is returned — not a cancellation it caused — and the
// others are cancelled rather than left running.
func TestSendJudgeBatchesFailsWholeAndCancelsTheRest(t *testing.T) {
	boom := errors.New("529 overloaded")
	var cancelled atomic.Int32
	j := &llmgate.MockJudge{Respond: func(ctx context.Context, req llmgate.JudgeRequest) (*llmgate.Judgment, error) {
		if _, ok := req.Questions["q0"]; ok {
			return nil, boom
		}
		select {
		case <-ctx.Done():
			cancelled.Add(1)
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return &llmgate.Judgment{}, nil
		}
	}}
	reqs := []llmgate.JudgeRequest{oneQuestion("q0"), oneQuestion("q1"), oneQuestion("q2")}
	res, err := sendJudgeBatches(context.Background(), j, reqs)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the originating failure", err)
	}
	if res != nil {
		t.Error("a failed phase must not return partial results")
	}
	if cancelled.Load() != 2 {
		t.Errorf("%d of 2 outstanding requests were cancelled", cancelled.Load())
	}
}

func TestSendJudgeBatchesEmpty(t *testing.T) {
	res, err := sendJudgeBatches(context.Background(), &llmgate.MockJudge{}, nil)
	if err != nil || len(res) != 0 {
		t.Errorf("got %v, %v", res, err)
	}
}
