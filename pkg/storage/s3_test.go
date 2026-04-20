package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
)

func TestNewS3Validation(t *testing.T) {
	t.Parallel()

	if _, err := NewS3(S3Config{}); err == nil {
		t.Error("empty config: expected error, got nil")
	}
	if _, err := NewS3(S3Config{Bucket: "b"}); err == nil {
		t.Error("no endpoint or region: expected error, got nil")
	}
	if _, err := NewS3(S3Config{Bucket: "b", Region: "us-east-1"}); err != nil {
		t.Errorf("region-only config should succeed, got: %v", err)
	}
	if _, err := NewS3(S3Config{Bucket: "b", Endpoint: "https://minio.local"}); err != nil {
		t.Errorf("endpoint-only config should succeed, got: %v", err)
	}
}

// fakeAPIError is a minimal smithy.APIError for verifying isNotFound's
// reliance on error codes rather than concrete SDK types.
type fakeAPIError struct{ code string }

func (e *fakeAPIError) Error() string                 { return e.code }
func (e *fakeAPIError) ErrorCode() string             { return e.code }
func (e *fakeAPIError) ErrorMessage() string          { return e.code }
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

var _ smithy.APIError = (*fakeAPIError)(nil)

func TestIsNotFound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("network blew up"), false},
		{"NoSuchKey code", &fakeAPIError{code: "NoSuchKey"}, true},
		{"NotFound code", &fakeAPIError{code: "NotFound"}, true},
		{"404 code", &fakeAPIError{code: "404"}, true},
		{"other AWS error", &fakeAPIError{code: "AccessDenied"}, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := isNotFound(c.err); got != c.want {
				t.Errorf("isNotFound(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestS3Integration exercises the full Put/Exists/Get/Delete path against a
// real S3-compatible endpoint. Gated on env so this stays out of the default
// `go test ./...` — set TEST_S3_* to run it.
//
//	TEST_S3_ENDPOINT=http://localhost:9000
//	TEST_S3_BUCKET=vectorless-test
//	TEST_S3_ACCESS_KEY=minioadmin
//	TEST_S3_SECRET_KEY=minioadmin
//	TEST_S3_PATH_STYLE=true
func TestS3Integration(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	bucket := os.Getenv("TEST_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("TEST_S3_ENDPOINT / TEST_S3_BUCKET not set; skipping S3 integration test")
	}

	s, err := NewS3(S3Config{
		Endpoint:     endpoint,
		Region:       envOr("TEST_S3_REGION", "us-east-1"),
		Bucket:       bucket,
		AccessKey:    os.Getenv("TEST_S3_ACCESS_KEY"),
		SecretKey:    os.Getenv("TEST_S3_SECRET_KEY"),
		UsePathStyle: os.Getenv("TEST_S3_PATH_STYLE") == "true",
	})
	if err != nil {
		t.Fatalf("new s3: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := "vle-test/" + t.Name() + "-" + time.Now().UTC().Format("20060102T150405.000")
	body := []byte("hello, vectorless\n")

	if err := s.Put(ctx, key, bytes.NewReader(body), Metadata{ContentType: "text/plain"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(context.Background(), key) })

	exists, err := s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !exists {
		t.Fatal("exists: expected true after put")
	}

	rc, meta, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
	if meta.ContentType != "" && !strings.HasPrefix(meta.ContentType, "text/plain") {
		t.Errorf("content type: got %q, want text/plain", meta.ContentType)
	}

	url, err := s.SignedURL(ctx, key, 5*time.Minute)
	if err != nil {
		t.Fatalf("signedurl: %v", err)
	}
	if !strings.Contains(url, bucket) {
		t.Errorf("signed url %q does not mention bucket %q", url, bucket)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: got %v, want ErrNotFound", err)
	}
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
