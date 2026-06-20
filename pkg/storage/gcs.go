package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	gcs "cloud.google.com/go/storage"
)

// GCSConfig configures the native Google Cloud Storage backend.
//
// Native GCS (vs the S3-compat shim) auths via Application Default
// Credentials — on Cloud Run that means the instance's service
// account, no HMAC keys or key files needed. This is the right choice
// when your org policy disables service-account key creation, which
// is the default on most managed orgs.
type GCSConfig struct {
	// Bucket is the GCS bucket name (no gs:// prefix).
	Bucket string
}

// GCS is a Storage backed by cloud.google.com/go/storage.
type GCS struct {
	cfg    GCSConfig
	client *gcs.Client
	bucket *gcs.BucketHandle
}

// NewGCS constructs a GCS Storage. Uses ADC; on Cloud Run that's the
// runtime service account injected by the metadata server, which
// must have roles/storage.objectAdmin (or equivalent) on the bucket.
func NewGCS(ctx context.Context, cfg GCSConfig) (*GCS, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("gcs storage: bucket is required")
	}
	client, err := gcs.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs storage: new client: %w", err)
	}
	return &GCS{
		cfg:    cfg,
		client: client,
		bucket: client.Bucket(cfg.Bucket),
	}, nil
}

// Put writes r to key. ContentType is forwarded if set in meta.
func (g *GCS) Put(ctx context.Context, key string, r io.Reader, meta Metadata) error {
	w := g.bucket.Object(key).NewWriter(ctx)
	if meta.ContentType != "" {
		w.ContentType = meta.ContentType
	}
	if len(meta.Custom) > 0 {
		w.Metadata = meta.Custom
	}
	if _, err := io.Copy(w, r); err != nil {
		// Best-effort close; the underlying write is already broken.
		_ = w.Close()
		return fmt.Errorf("gcs storage: put %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs storage: close %q: %w", key, err)
	}
	return nil
}

// Get streams the object bytes. Returns ErrNotFound if absent.
func (g *GCS) Get(ctx context.Context, key string) (io.ReadCloser, Metadata, error) {
	obj := g.bucket.Object(key)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return nil, Metadata{}, fmt.Errorf("%w: gs://%s/%s", ErrNotFound, g.cfg.Bucket, key)
		}
		return nil, Metadata{}, fmt.Errorf("gcs storage: attrs %q: %w", key, err)
	}
	rc, err := obj.NewReader(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return nil, Metadata{}, fmt.Errorf("%w: gs://%s/%s", ErrNotFound, g.cfg.Bucket, key)
		}
		return nil, Metadata{}, fmt.Errorf("gcs storage: get %q: %w", key, err)
	}
	return rc, Metadata{
		Key:         key,
		Size:        attrs.Size,
		ContentType: attrs.ContentType,
		ETag:        attrs.Etag,
		ModifiedAt:  attrs.Updated,
		Custom:      attrs.Metadata,
	}, nil
}

// Delete removes the object. Returns ErrNotFound if it didn't exist.
func (g *GCS) Delete(ctx context.Context, key string) error {
	err := g.bucket.Object(key).Delete(ctx)
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return fmt.Errorf("%w: gs://%s/%s", ErrNotFound, g.cfg.Bucket, key)
	}
	if err != nil {
		return fmt.Errorf("gcs storage: delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether the object is present.
func (g *GCS) Exists(ctx context.Context, key string) (bool, error) {
	_, err := g.bucket.Object(key).Attrs(ctx)
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("gcs storage: exists %q: %w", key, err)
	}
	return true, nil
}

// SignedURL returns a V4-signed URL valid for expiry. Requires the
// runtime SA to have iam.serviceAccounts.signBlob on itself (granted
// by the Service Account Token Creator role on the SA itself), which
// is the standard pattern on Cloud Run.
func (g *GCS) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	url, err := g.bucket.SignedURL(key, &gcs.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(expiry),
		Scheme:  gcs.SigningSchemeV4,
	})
	if err != nil {
		// SignedURL needs either a private key in credentials or
		// signBlob permissions on the runtime SA. If neither is
		// available we fall back to "no signed URL" rather than
		// breaking the caller — they can proxy through the engine.
		return "", nil
	}
	return url, nil
}
