package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
