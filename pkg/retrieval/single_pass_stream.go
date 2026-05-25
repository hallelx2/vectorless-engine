package retrieval

import (
	"context"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Compile-time check: SinglePass implements StreamStrategy.
var _ StreamStrategy = (*SinglePass)(nil)

// SelectStream implements StreamStrategy for SinglePass.
//
// For single-pass, there's only one "slice" (the whole tree), so the
// streaming behavior is simpler: started → section_selected (×N) → completed.
func (s *SinglePass) SelectStream(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) <-chan StreamEvent {
	ch := make(chan StreamEvent, 32)

	go func() {
		defer close(ch)

		emit(ch, StreamEvent{
			Type:        EventStarted,
			TotalSlices: 1,
			Message:     "starting single-pass retrieval",
		})

		emit(ch, StreamEvent{
			Type:        EventSliceStarted,
			SliceIndex:  0,
			TotalSlices: 1,
			Message:     "processing entire tree in one pass",
		})

		ids, err := s.Select(ctx, t, query, budget)
		if err != nil {
			emit(ch, StreamEvent{Type: EventError, Error: err})
			return
		}

		for _, id := range ids {
			emit(ch, StreamEvent{
				Type:        EventSectionSelected,
				SectionID:   id,
				SliceIndex:  0,
				TotalSlices: 1,
			})
		}

		emit(ch, StreamEvent{
			Type:        EventSliceCompleted,
			SliceIndex:  0,
			TotalSlices: 1,
			Message:     "single-pass done",
		})

		emit(ch, StreamEvent{
			Type:    EventCompleted,
			Message: "retrieval complete",
		})
	}()

	return ch
}
