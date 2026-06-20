package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalResolvesRootToAbsolute(t *testing.T) {
	// A relative root must be pinned to an absolute path so the worker reads
	// from the same place regardless of the process's working directory at
	// read time (the HAL-321 "object not found on a file that is on disk" bug).
	dir := t.TempDir()
	rel, err := filepath.Rel(mustGetwd(t), dir)
	if err != nil {
		t.Skipf("cannot relativise temp dir: %v", err)
	}
	l, err := NewLocal(rel)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if !filepath.IsAbs(l.Root()) {
		t.Fatalf("Root() = %q, want absolute", l.Root())
	}
}

func TestLocalGetNotFoundCarriesPath(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	_, _, err = l.Get(context.Background(), "documents/missing/source.pdf")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// The error must name the resolved path so an operator can immediately see
	// WHERE the worker looked.
	if !strings.Contains(err.Error(), "source.pdf") {
		t.Fatalf("not-found error %q should include the resolved path", err)
	}
}

func TestLocalDeleteNotFoundCarriesPath(t *testing.T) {
	// Delete of a missing key must also report ErrNotFound WITH the resolved
	// path. Previously it returned the bare sentinel, which is indistinguishable
	// in a DB error column from a Get miss — exactly the HAL-323 ambiguity where
	// pathless "object not found" rows could not be attributed to a code path.
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	err = l.Delete(context.Background(), "documents/missing/source.pdf")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "source.pdf") {
		t.Fatalf("not-found error %q should include the resolved path", err)
	}
}

func TestLocalGetRetriesTransientNotExist(t *testing.T) {
	// Simulate the Windows Defender scan window: the source does not exist when
	// Get is first called, then appears shortly after. On Windows the bounded
	// internal retry (cumulative ~1.4s) must ride through the gap so the FIRST
	// Get returns the bytes — no caller-side fallback. This test fails if the
	// Windows retry loop is removed or shortened below the write delay, because
	// a single-pass Get would return ErrNotFound at t≈0. Off Windows there is no
	// scan-hold and Get makes a single pass, so we wait for the writer first.
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	key := "documents/doc-late/source.pdf"
	want := []byte("appears late")

	done := make(chan struct{})
	go func() {
		// Write the file a beat after Get starts polling — well within the
		// Windows retry budget but after the first attempt would have missed.
		time.Sleep(80 * time.Millisecond)
		_ = l.Put(ctx, key, bytes.NewReader(want), Metadata{})
		close(done)
	}()

	if runtime.GOOS != "windows" {
		<-done // single-pass platforms: ensure the file is present first
	}

	// On Windows this is the assertion that matters: the internal retry must
	// make this succeed despite the file being absent when the call began.
	rc, _, err := l.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get rode through the write window? err = %v (want nil — retry should cover the %v file-appear delay)", err, 80*time.Millisecond)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip = %q, want %q", got, want)
	}
	<-done // avoid leaking the writer goroutine
}

func TestLocalPutThenGetRoundTrip(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	want := []byte("hello vectorless")
	if err := l.Put(ctx, "documents/doc1/source.pdf", bytes.NewReader(want), Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, _, err := l.Get(ctx, "documents/doc1/source.pdf")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip = %q, want %q", got, want)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}
