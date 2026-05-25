package retrieval

import (
	"context"

	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// StreamEvent is a single event emitted during streaming retrieval.
// The server sends these to the client as they arrive.
type StreamEvent struct {
	// Type classifies the event.
	Type StreamEventType

	// SectionID is set when Type is EventSectionSelected.
	SectionID tree.SectionID

	// SliceIndex identifies which chunk produced this event (chunked-tree).
	// -1 for single-pass.
	SliceIndex int

	// TotalSlices is the total number of slices being processed.
	TotalSlices int

	// Message is a human-readable status update (e.g. "processing slice 3/8").
	Message string

	// Error is set when Type is EventError.
	Error error
}

// StreamEventType classifies a streaming retrieval event.
type StreamEventType string

const (
	// EventStarted signals the strategy run has begun.
	EventStarted StreamEventType = "started"

	// EventSliceStarted signals a slice is being sent to the LLM.
	EventSliceStarted StreamEventType = "slice_started"

	// EventSectionSelected signals a section was selected by the model.
	EventSectionSelected StreamEventType = "section_selected"

	// EventSliceCompleted signals a slice finished processing.
	EventSliceCompleted StreamEventType = "slice_completed"

	// EventCompleted signals the full strategy run is done.
	EventCompleted StreamEventType = "completed"

	// EventError signals an error occurred.
	EventError StreamEventType = "error"
)

// StreamStrategy extends Strategy with a streaming Select variant.
// Strategies that support streaming implement this interface in
// addition to Strategy. The server checks for it at runtime.
type StreamStrategy interface {
	Strategy

	// SelectStream works like Select but emits events to the channel
	// as sections are identified. The channel is closed when the
	// strategy finishes (or errors). The caller should drain the
	// channel to completion.
	//
	// The returned channel is buffered. The strategy will not block
	// on sends if the consumer is slow — it will drop events rather
	// than stall the LLM pipeline.
	SelectStream(ctx context.Context, t *tree.Tree, query string, budget ContextBudget) <-chan StreamEvent
}
