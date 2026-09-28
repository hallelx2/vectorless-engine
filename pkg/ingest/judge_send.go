package ingest

import (
	"context"
	"sync"

	"github.com/hallelx2/llmgate"
)

// sendJudgeBatches sends independent Judge requests all at once and
// returns the answers in request order: results[i] answers reqs[i].
//
// Every TOC phase used to build one batch, send it, wait, and only then
// build the next, so a phase's wall clock was the sum of its requests
// rather than the longest of them (HAL-1545). The batches never depended
// on each other — each carries its own state and its own questions — so
// the wait bought nothing. How many are actually in flight is the
// provider's adaptive limiter's call (HAL-1372), not a loop's.
//
// On the first failure the rest are cancelled and that error is
// returned, never a partial set: every caller abandons its phase on any
// failure and falls back, and a phase that quietly used half its answers
// would read as "nothing found" for the other half.
func sendJudgeBatches(ctx context.Context, j llmgate.Judge, reqs []llmgate.JudgeRequest) ([]*llmgate.Judgment, error) {
	out := make([]*llmgate.Judgment, len(reqs))
	switch len(reqs) {
	case 0:
		return out, nil
	case 1:
		res, err := j.Judge(ctx, reqs[0])
		if err != nil {
			return nil, err
		}
		out[0] = res
		return out, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := j.Judge(ctx, reqs[i])
			if err != nil {
				// The first failure wins; the cancellations it causes in
				// the others are consequences, not causes.
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			out[i] = res
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
