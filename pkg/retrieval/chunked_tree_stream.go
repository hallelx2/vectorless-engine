package retrieval

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Compile-time check: ChunkedTree implements StreamStrategy.
var _ StreamStrategy = (*ChunkedTree)(nil)

// SelectStream implements StreamStrategy for ChunkedTree.
//
// It splits the tree, processes slices in parallel (like Select), but
// emits StreamEvents to the returned channel as each slice completes
// and sections are identified. The caller sees results incrementally
// instead of waiting for the full merge.
//
// The channel is closed when the strategy finishes.
func (c *ChunkedTree) SelectStream(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) <-chan StreamEvent {
	ch := make(chan StreamEvent, 64)

	go func() {
		defer close(ch)

		// Emit start.
		tok := LLMTokenizer{C: c.LLM}
		slices, err := c.Splitter.Split(ctx, t, budget, tok)
		if err != nil {
			emit(ch, StreamEvent{Type: EventError, Error: err})
			return
		}

		emit(ch, StreamEvent{
			Type:        EventStarted,
			TotalSlices: len(slices),
			Message:     fmt.Sprintf("starting retrieval: %d slices", len(slices)),
		})

		maxPar := budget.MaxParallelCalls
		if maxPar <= 0 {
			maxPar = 8
		}

		sem := make(chan struct{}, maxPar)
		g, gctx := errgroup.WithContext(ctx)

		for i, sl := range slices {
			i, sl := i, sl
			g.Go(func() error {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-gctx.Done():
					return gctx.Err()
				}

				emit(ch, StreamEvent{
					Type:        EventSliceStarted,
					SliceIndex:  i,
					TotalSlices: len(slices),
					Message:     fmt.Sprintf("processing slice %d/%d", i+1, len(slices)),
				})

				ids, err := c.reasonOverSlice(gctx, sl, query, budget)
				if err != nil {
					return err
				}

				// Emit each selected section individually.
				for _, id := range ids {
					emit(ch, StreamEvent{
						Type:        EventSectionSelected,
						SectionID:   id,
						SliceIndex:  i,
						TotalSlices: len(slices),
					})
				}

				emit(ch, StreamEvent{
					Type:        EventSliceCompleted,
					SliceIndex:  i,
					TotalSlices: len(slices),
					Message:     fmt.Sprintf("slice %d/%d done: %d sections", i+1, len(slices), len(ids)),
				})

				return nil
			})
		}

		if err := g.Wait(); err != nil {
			emit(ch, StreamEvent{Type: EventError, Error: err})
			return
		}

		emit(ch, StreamEvent{
			Type:    EventCompleted,
			Message: "retrieval complete",
		})
	}()

	return ch
}

// emit sends an event without blocking. If the channel is full the
// event is dropped — we never stall the LLM pipeline waiting on a
// slow consumer.
func emit(ch chan<- StreamEvent, e StreamEvent) {
	select {
	case ch <- e:
	default:
		// Consumer is slow; drop this event rather than blocking.
	}
}
