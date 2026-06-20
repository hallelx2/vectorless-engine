package ingest

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The classifier decides whether a parser failure is worth retrying. Getting
// this wrong is expensive in both directions: marking a transient timeout
// permanent dead-letters a document that would have parsed on a quieter
// attempt; marking a deterministic rejection transient burns the whole retry
// budget (and, as seen in HAL-321, interleaves confusing "object not found"
// errors into the job history).
func TestIsTransientParseErr(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		transient bool
	}{
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, true},
		{"wrapped deadline", fmt.Errorf("pdf: parse cancelled: %w", context.DeadlineExceeded), true},
		{"pdf timeout message", errors.New("pdf: parse exceeded 5m0s — document too complex or malformed"), true},
		{"pdf cancelled message", errors.New("pdf: parse cancelled: context deadline exceeded"), true},
		{"encrypted", errors.New("pdf: open: encrypted and could not be unlocked with empty password: x"), false},
		{"backend rejected", errors.New("pdf: open: ledongthuc/pdf backend rejected the document: malformed PDF: 256-bit encryption key"), false},
		{"no extractable text", errors.New("pdf: parsed but no extractable text — the document may be a scanned image"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientParseErr(tc.err); got != tc.transient {
				t.Fatalf("isTransientParseErr(%q) = %v, want %v", tc.err, got, tc.transient)
			}
		})
	}
}
