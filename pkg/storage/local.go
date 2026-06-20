package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Local is a filesystem-backed Storage. Good for dev and small self-hosters
// running on a single node.
type Local struct {
	root string
}

// NewLocal returns a Local storage rooted at dir. The directory is created
// if it does not exist.
//
// dir is resolved to an ABSOLUTE path up front. A relative root (the default
// is "./data/documents") is otherwise resolved against the process's current
// working directory on every call — so if the engine is ever relaunched from a
// different directory while the queue still holds jobs that reference earlier
// uploads (River persists jobs in Postgres across restarts), the worker would
// look under a different root than the one the bytes were written to and fail
// with "object not found" on a file that is in fact on disk. Pinning the root
// to an absolute path at construction makes it stable for the process lifetime.
func NewLocal(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &Local{root: abs}, nil
}

// Root returns the absolute filesystem path the storage is rooted at. Exposed
// so the engine can log the resolved root at boot (the single most useful fact
// when diagnosing an "object not found" on a file that appears to be on disk).
func (l *Local) Root() string { return l.root }

func (l *Local) path(key string) string {
	// Keys may include slashes; treat them as path separators.
	return filepath.Join(l.root, filepath.FromSlash(key))
}

func (l *Local) Put(ctx context.Context, key string, r io.Reader, _ Metadata) error {
	full := l.path(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	// fsync before returning. Ingest enqueues the background parse job
	// immediately after Put returns; the worker may pick it up within
	// microseconds and Stat this exact path. Without the sync the bytes
	// (and on Windows the directory entry) can lag behind, so the worker
	// races the write and fails with ErrNotFound on a file that is in
	// fact being written. Syncing here makes the object durably visible
	// before the caller proceeds to enqueue.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, Metadata, error) {
	full := l.path(key)

	// On Windows, Defender real-time protection holds a brief lock on a
	// freshly-written file while it scans it; during that window GetFileAttributes
	// (os.Stat) and CreateFile (os.Open) return ERROR_FILE_NOT_FOUND on a file
	// that IS on disk (HAL-321/HAL-323). The same flip can hit between our Stat
	// and Open (a TOCTOU race). A short bounded retry here closes that window
	// transparently so the caller never sees the transient as a hard not-found.
	// On non-Windows there is no such hold, so we make a single pass.
	const winRetries = 8
	attempts := 1
	if runtime.GOOS == "windows" {
		attempts = winRetries
	}

	for i := 0; i < attempts; i++ {
		info, err := os.Stat(full)
		if err == nil {
			f, openErr := os.Open(full)
			if openErr == nil {
				return f, Metadata{
					Key:        key,
					Size:       info.Size(),
					ModifiedAt: info.ModTime(),
				}, nil
			}
			// Open can fail with not-exist even though Stat just succeeded — the
			// Windows scan window flipped between the two calls. Treat it like a
			// transient stat miss and retry; surface anything else immediately.
			if !errors.Is(openErr, os.ErrNotExist) {
				return nil, Metadata{}, openErr
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, Metadata{}, err
		}

		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return nil, Metadata{}, ctx.Err()
			case <-time.After(time.Duration(i+1) * 50 * time.Millisecond):
			}
		}
	}

	// Wrap ErrNotFound (errors.Is still matches) but carry the resolved absolute
	// path so the failure is self-diagnosing: a caller seeing this in a log can
	// immediately tell whether it looked in the wrong root vs. the bytes
	// genuinely being absent.
	return nil, Metadata{}, fmt.Errorf("%w: %s", ErrNotFound, full)
}

func (l *Local) Delete(ctx context.Context, key string) error {
	full := l.path(key)
	err := os.Remove(full)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, full)
	}
	return err
}

func (l *Local) Exists(ctx context.Context, key string) (bool, error) {
	_, err := os.Stat(l.path(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// SignedURL is not supported by the filesystem backend. Callers should
// proxy downloads through the engine when this returns the empty string.
func (l *Local) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "", nil
}
