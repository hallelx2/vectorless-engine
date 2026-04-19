package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// S3Config configures an S3-compatible Storage. The same implementation
// serves AWS S3, Cloudflare R2, MinIO, Backblaze B2, DigitalOcean Spaces,
// and any other provider that speaks S3.
type S3Config struct {
	Endpoint     string // "https://s3.amazonaws.com" for AWS, account URL for R2, etc.
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool // true for MinIO / R2; false for AWS virtual-hosted style
}

// S3 is an S3-compatible Storage.
//
// The actual AWS SDK wiring will be filled in once the main service is
// running; leaving it as a stub here keeps module dependencies light during
// scaffolding. Implementations of the six Storage methods will use
// github.com/aws/aws-sdk-go-v2/service/s3.
type S3 struct {
	cfg S3Config
}

// NewS3 returns a new S3-compatible Storage.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3 storage: bucket is required")
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("s3 storage: endpoint is required")
	}
	return &S3{cfg: cfg}, nil
}

// The following methods are intentionally stubs for the scaffold. They will
// be implemented against aws-sdk-go-v2 when the engine gains ingest/query
// functionality in Phase 1.

func (s *S3) Put(ctx context.Context, key string, r io.Reader, meta Metadata) error {
	return errors.New("s3 storage: Put not yet implemented")
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Metadata, error) {
	return nil, Metadata{}, errors.New("s3 storage: Get not yet implemented")
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return errors.New("s3 storage: Delete not yet implemented")
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	return false, errors.New("s3 storage: Exists not yet implemented")
}

func (s *S3) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "", errors.New("s3 storage: SignedURL not yet implemented")
}
