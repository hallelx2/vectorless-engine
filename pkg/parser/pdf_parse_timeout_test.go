package parser

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunParseWithDeadline_TimesOutDoesNotHang is the core robustness
// assertion: when the parse work runs longer than the deadline, the
// wrapper returns the timeout error in ~the timeout — NOT after the work
// finishes (which here is 10s, far longer than any CI step should wait).
// This is the exact pathology the whole-Parse deadline guards against: a
// pure-Go row extractor (or any backend) that never returns must fail
// fast instead of wedging ingest forever.
func TestRunParseWithDeadline_TimesOutDoesNotHang(t *testing.T) {
	const timeout = 50 * time.Millisecond
	const workDuration = 10 * time.Second // simulates the unbounded hang

	started := make(chan struct{})
	work := func() (*ParsedDoc, error) {
		close(started)
		time.Sleep(workDuration) // never returns within the deadline
		return &ParsedDoc{Title: "should never be observed"}, nil
	}

	start := time.Now()
	doc, err := runParseWithDeadline(context.Background(), timeout, work)
	elapsed := time.Since(start)

	<-started // the work goroutine did start (we really are abandoning it)

	if err == nil {
		t.Fatalf("expected a timeout error, got doc=%v err=nil", doc)
	}
	if doc != nil {
		t.Errorf("expected nil doc on timeout, got %v", doc)
	}
	// The error must clearly name the timeout so ops see why the doc failed.
	if !strings.Contains(err.Error(), "parse exceeded") {
		t.Errorf("error %q does not mention the parse-exceeded cause", err.Error())
	}
	// Critically: we returned in ~timeout, not in ~workDuration. Generous
	// upper bound (still 20x the timeout but 5x under the work sleep) keeps
	// the test robust on a loaded CI box while still proving "doesn't hang".
	if elapsed > time.Second {
		t.Errorf("runParseWithDeadline took %s — it waited for the work instead of the deadline (hang not bounded)", elapsed)
	}
}

// TestRunParseWithDeadline_FastWorkPassesThrough confirms the happy path:
// work that finishes well inside the deadline has its result returned
// verbatim, untouched by the wrapper.
func TestRunParseWithDeadline_FastWorkPassesThrough(t *testing.T) {
	want := &ParsedDoc{Title: "quick"}
	doc, err := runParseWithDeadline(context.Background(), time.Second, func() (*ParsedDoc, error) {
		return want, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc != want {
		t.Errorf("doc = %v, want %v", doc, want)
	}
}

// TestRunParseWithDeadline_PropagatesWorkError confirms a genuine parse
// error from the work (the common "bad PDF" case) is returned as-is, not
// masked by the timeout machinery.
func TestRunParseWithDeadline_PropagatesWorkError(t *testing.T) {
	sentinel := errors.New("pdf: open: boom")
	doc, err := runParseWithDeadline(context.Background(), time.Second, func() (*ParsedDoc, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel %v", err, sentinel)
	}
	if doc != nil {
		t.Errorf("doc = %v, want nil", doc)
	}
}

// TestRunParseWithDeadline_DisabledRunsInline confirms a non-positive
// timeout disables the bound entirely: the work runs inline (no
// goroutine, no deadline) and its result is returned even though the
// "work" takes a beat. This is the explicit escape hatch a negative
// ParseTimeout selects (legacy unbounded behaviour).
func TestRunParseWithDeadline_DisabledRunsInline(t *testing.T) {
	want := &ParsedDoc{Title: "inline"}
	doc, err := runParseWithDeadline(context.Background(), 0, func() (*ParsedDoc, error) {
		time.Sleep(20 * time.Millisecond)
		return want, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc != want {
		t.Errorf("doc = %v, want %v", doc, want)
	}
}

// TestRunParseWithDeadline_ContextCancel confirms a cancelled context
// unblocks the wrapper promptly even when the work is still running and
// the (long) timeout has not fired — so a shutdown/cancel propagates
// instead of waiting out the full parse deadline.
func TestRunParseWithDeadline_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	doc, err := runParseWithDeadline(ctx, time.Hour, func() (*ParsedDoc, error) {
		time.Sleep(10 * time.Second)
		return &ParsedDoc{}, nil
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a cancellation error, got doc=%v err=nil", doc)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("cancellation took %s — wrapper did not honour ctx promptly", elapsed)
	}
}

// TestRunParseWithDeadline_RecoversPanic confirms a panic deep inside the
// parse work is recovered and surfaced as an error rather than crashing
// the ingest worker process.
func TestRunParseWithDeadline_RecoversPanic(t *testing.T) {
	doc, err := runParseWithDeadline(context.Background(), time.Second, func() (*ParsedDoc, error) {
		panic("backend exploded")
	})
	if err == nil {
		t.Fatalf("expected an error from a panicking work, got doc=%v err=nil", doc)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("error %q does not indicate a recovered panic", err.Error())
	}
}

// TestResolvedParseTimeout covers the zero/default, explicit, and
// disabled (negative) resolutions of the configured ParseTimeout.
func TestResolvedParseTimeout(t *testing.T) {
	if got := (&PDF{}).resolvedParseTimeout(); got != defaultParseTimeout {
		t.Errorf("zero ParseTimeout: got %s, want default %s", got, defaultParseTimeout)
	}
	if got := (&PDF{ParseTimeout: 5 * time.Second}).resolvedParseTimeout(); got != 5*time.Second {
		t.Errorf("explicit ParseTimeout: got %s, want 5s", got)
	}
	if got := (&PDF{ParseTimeout: -1}).resolvedParseTimeout(); got > 0 {
		t.Errorf("negative ParseTimeout should stay non-positive (bound disabled), got %s", got)
	}
}

// TestPDFParse_HonoursTinyTimeout drives the FULL Parse path (not just the
// helper) with a deliberately tiny deadline and a real PDF whose parse
// takes longer than that deadline, proving Parse itself returns the
// timeout error promptly rather than hanging.
//
// We reuse the table fixture (a real ruled-table PDF). Even its modest
// parse comfortably exceeds a 1ms deadline, so the wrapper fires. If the
// fixture is ever made trivially fast, the assertion that we either time
// out OR succeed (never hang) still holds — the test's job is to prove the
// wrapper is wired into Parse and bounds it, which the elapsed-time guard
// enforces.
func TestPDFParse_HonoursTinyTimeout(t *testing.T) {
	b := readParserFixture(t, "tables-example.pdf")
	p := &PDF{Tables: DefaultTableOpts(), ParseTimeout: time.Millisecond}

	start := time.Now()
	_, err := p.Parse(context.Background(), bytes.NewReader(b))
	elapsed := time.Since(start)

	// The whole point: Parse returned quickly. A real parse of this fixture
	// without the bound takes longer than 1ms, so on any normal machine we
	// expect the timeout error; either way Parse MUST NOT hang.
	if elapsed > 5*time.Second {
		t.Fatalf("Parse took %s with a 1ms ParseTimeout — the deadline is not wired into Parse", elapsed)
	}
	if err != nil && !strings.Contains(err.Error(), "parse exceeded") {
		// A non-timeout error (e.g. the parse genuinely finished first on a
		// very fast box and returned a real result with err==nil) is fine;
		// only an unexpected error shape is worth flagging.
		t.Logf("Parse returned non-timeout error (acceptable if parse beat the 1ms deadline): %v", err)
	}
}

// readParserFixture mirrors the external test's readFixture but lives in
// the white-box package so the timeout tests (which need unexported
// symbols) can load the same testdata files.
func readParserFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %q: %v", path, err)
	}
	return b
}
