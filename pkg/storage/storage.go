// Package storage abstracts where document bytes live.
//
// The engine never assumes a particular backend. Tests and small self-hosters
// use Local; production commonly uses S3-compatible storage (AWS S3,
// Cloudflare R2, MinIO, Backblaze B2, DigitalOcean Spaces). Future backends:
// Google Cloud Storage, Azure Blob.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Get/Delete when a key does not exist.
var ErrNotFound = errors.New("storage: object not found")

// Metadata describes a stored object.
type Metadata struct {
	Key         string
	Size        int64
	ContentType string
	ETag        string
	ModifiedAt  time.Time
	Custom      map[string]string
}

// Storage is the contract every backend must satisfy.
//
// Implementations MUST be safe for concurrent use by multiple goroutines.
type Storage interface {
	// Put writes data to key. If the key exists it is overwritten.
	Put(ctx context.Context, key string, r io.Reader, meta Metadata) error

	// Get reads data at key. Caller is responsible for closing the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, Metadata, error)

	// Delete removes key. Returns ErrNotFound if the key does not exist.
	Delete(ctx context.Context, key string) error

	// Exists reports whether key exists.
	Exists(ctx context.Context, key string) (bool, error)

	// SignedURL returns a time-limited URL that allows direct reading of the
	// object by a client. Backends that don't natively support signed URLs
	// should return an empty string and a nil error, letting callers fall
	// back to proxying through the engine.
	SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error)
}
