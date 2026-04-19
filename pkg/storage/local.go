package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Local is a filesystem-backed Storage. Good for dev and small self-hosters
// running on a single node.
type Local struct {
	root string
}

// NewLocal returns a Local storage rooted at dir. The directory is created
// if it does not exist.
func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &Local{root: dir}, nil
}

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
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, Metadata, error) {
	full := l.path(key)
	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Metadata{}, ErrNotFound
		}
		return nil, Metadata{}, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, Metadata{}, err
	}
	return f, Metadata{
		Key:        key,
		Size:       info.Size(),
		ModifiedAt: info.ModTime(),
	}, nil
}

func (l *Local) Delete(ctx context.Context, key string) error {
	err := os.Remove(l.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
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
