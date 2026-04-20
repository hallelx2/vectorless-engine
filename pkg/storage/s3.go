package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
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

// S3 is an S3-compatible Storage backed by aws-sdk-go-v2.
//
// The same type handles every S3-compatible vendor because the differences
// all boil down to three knobs: Endpoint, Region, and UsePathStyle. A
// presigner is kept alongside the client so SignedURL doesn't need to
// reconstruct one per call.
type S3 struct {
	cfg       S3Config
	client    *s3.Client
	presigner *s3.PresignClient
}

// NewS3 returns a new S3-compatible Storage. It constructs the SDK client
// eagerly so misconfiguration surfaces at boot, not on the first Put.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3 storage: bucket is required")
	}
	// Endpoint is required for non-AWS vendors; AWS itself is happy with
	// region alone. We don't guess — force the caller to be explicit.
	if cfg.Endpoint == "" && cfg.Region == "" {
		return nil, errors.New("s3 storage: endpoint or region is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1" // placeholder some non-AWS vendors require
	}

	// Loading with LoadDefaultConfig picks up the standard AWS credential
	// chain (env, shared config, IAM role) when AccessKey/SecretKey are
	// empty — useful for AWS IRSA / instance profile deployments.
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3 storage: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})

	return &S3{
		cfg:       cfg,
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

// Put uploads r to key. Content-Type and custom metadata are forwarded.
// Size from meta is advisory — the SDK streams r either way.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, meta Metadata) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
		Body:   r,
	}
	if meta.ContentType != "" {
		in.ContentType = aws.String(meta.ContentType)
	}
	if len(meta.Custom) > 0 {
		in.Metadata = meta.Custom
	}
	if _, err := s.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3 storage: put %q: %w", key, err)
	}
	return nil
}

// Get fetches key. Caller closes the returned reader. Missing keys map to
// storage.ErrNotFound so downstream handlers can branch on a sentinel.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Metadata, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, Metadata{}, ErrNotFound
		}
		return nil, Metadata{}, fmt.Errorf("s3 storage: get %q: %w", key, err)
	}
	meta := Metadata{
		Key:    key,
		Size:   aws.ToInt64(out.ContentLength),
		Custom: out.Metadata,
	}
	if out.ContentType != nil {
		meta.ContentType = *out.ContentType
	}
	if out.ETag != nil {
		meta.ETag = *out.ETag
	}
	if out.LastModified != nil {
		meta.ModifiedAt = *out.LastModified
	}
	return out.Body, meta, nil
}

// Delete removes key. A missing key is reported as ErrNotFound even though
// S3 itself is idempotent — callers of storage.Storage expect this signal.
func (s *S3) Delete(ctx context.Context, key string) error {
	// S3 DeleteObject doesn't error on missing keys, so probe first.
	exists, err := s.Exists(ctx, key)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("s3 storage: delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether key exists via HEAD.
func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("s3 storage: head %q: %w", key, err)
}

// SignedURL returns a GET presigned URL good for expiry. Useful for handing
// sections directly to browsers without streaming through the engine.
func (s *S3) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	if expiry <= 0 {
		expiry = 15 * time.Minute
	}
	out, err := s.presigner.PresignGetObject(ctx,
		&s3.GetObjectInput{
			Bucket: aws.String(s.cfg.Bucket),
			Key:    aws.String(key),
		},
		s3.WithPresignExpires(expiry),
	)
	if err != nil {
		return "", fmt.Errorf("s3 storage: presign %q: %w", key, err)
	}
	return out.URL, nil
}

// isNotFound recognizes the two shapes S3 uses for "key doesn't exist":
// typed NoSuchKey on GET and smithy's generic NotFound on HEAD. Either
// collapses to our ErrNotFound so the rest of the stack only checks one
// sentinel.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code := ae.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound" || code == "404"
	}
	return false
}
