package queue

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// signJWT produces an HS256 JWT with the given claims, signed by key.
// Mirrors what QStash itself emits so tests exercise the real code path.
func signJWT(t *testing.T, key string, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(hdr)
	p := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(h + "." + p))
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func newTestVerifier(t *testing.T, cur, next string, now time.Time) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierConfig{
		CurrentSigningKey: cur,
		NextSigningKey:    next,
	})
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return now }
	return v
}

func TestVerifier_HappyPath(t *testing.T) {
	const key = "sig_current"
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"kind":"ingest_document","payload":{}}`)
	url := "https://engine.example.com/internal/jobs/ingest_document"

	jwt := signJWT(t, key, map[string]any{
		"iss":  "Upstash",
		"sub":  url,
		"exp":  now.Add(5 * time.Minute).Unix(),
		"nbf":  now.Add(-time.Minute).Unix(),
		"iat":  now.Unix(),
		"jti":  "unique-id",
		"body": bodyHash(body),
	})

	v := newTestVerifier(t, key, "", now)
	if err := v.Verify(jwt, body, url); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestVerifier_NextKeyRotation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte("anything")
	url := "https://engine.example.com/internal/jobs/build_tree"

	// Signed with the NEXT key (simulating mid-rotation: QStash has moved
	// to the new key, the engine still has both configured).
	jwt := signJWT(t, "sig_next", map[string]any{
		"sub":  url,
		"exp":  now.Add(time.Minute).Unix(),
		"body": bodyHash(body),
	})

	v := newTestVerifier(t, "sig_current", "sig_next", now)
	if err := v.Verify(jwt, body, url); err != nil {
		t.Fatalf("rotation: expected ok, got %v", err)
	}
}

func TestVerifier_WrongKey(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte("x")
	url := "https://engine.example.com/internal/jobs/x"

	jwt := signJWT(t, "imposter", map[string]any{
		"sub":  url,
		"exp":  now.Add(time.Minute).Unix(),
		"body": bodyHash(body),
	})

	v := newTestVerifier(t, "sig_current", "sig_next", now)
	err := v.Verify(jwt, body, url)
	if err == nil || !strings.Contains(err.Error(), "signature mismatch") {
		t.Fatalf("expected signature mismatch, got %v", err)
	}
}

func TestVerifier_TamperedBody(t *testing.T) {
	const key = "k"
	now := time.Unix(1_700_000_000, 0)
	originalBody := []byte(`{"amount":100}`)
	tamperedBody := []byte(`{"amount":9000}`)
	url := "https://engine.example.com/internal/jobs/x"

	jwt := signJWT(t, key, map[string]any{
		"sub":  url,
		"exp":  now.Add(time.Minute).Unix(),
		"body": bodyHash(originalBody),
	})

	v := newTestVerifier(t, key, "", now)
	// Signature is intact (JWT itself is valid) but the body was swapped
	// in flight — body-hash claim no longer matches.
	err := v.Verify(jwt, tamperedBody, url)
	if err == nil || !strings.Contains(err.Error(), "body hash mismatch") {
		t.Fatalf("expected body hash mismatch, got %v", err)
	}
}

func TestVerifier_Expired(t *testing.T) {
	const key = "k"
	now := time.Unix(1_700_000_000, 0)
	body := []byte("x")
	url := "https://engine.example.com/internal/jobs/x"

	jwt := signJWT(t, key, map[string]any{
		"sub":  url,
		"exp":  now.Add(-time.Minute).Unix(), // already expired
		"body": bodyHash(body),
	})

	v := newTestVerifier(t, key, "", now)
	err := v.Verify(jwt, body, url)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired, got %v", err)
	}
}

func TestVerifier_WrongURL(t *testing.T) {
	const key = "k"
	now := time.Unix(1_700_000_000, 0)
	body := []byte("x")

	// Signed for endpoint A, replayed to endpoint B.
	jwt := signJWT(t, key, map[string]any{
		"sub":  "https://engine.example.com/internal/jobs/ingest_document",
		"exp":  now.Add(time.Minute).Unix(),
		"body": bodyHash(body),
	})

	v := newTestVerifier(t, key, "", now)
	err := v.Verify(jwt, body, "https://engine.example.com/internal/jobs/reindex_document")
	if err == nil || !strings.Contains(err.Error(), "sub") {
		t.Fatalf("expected sub mismatch, got %v", err)
	}
}

func TestVerifier_MalformedJWT(t *testing.T) {
	v := newTestVerifier(t, "k", "", time.Now())
	if err := v.Verify("not.a.jwt.at.all", []byte("x"), ""); err == nil {
		t.Fatal("expected error on malformed jwt")
	}
	if err := v.Verify("", []byte("x"), ""); err == nil {
		t.Fatal("expected error on empty signature")
	}
}

func TestVerifier_SkipURLCheckWhenEmpty(t *testing.T) {
	const key = "k"
	now := time.Unix(1_700_000_000, 0)
	body := []byte("x")

	jwt := signJWT(t, key, map[string]any{
		"sub":  "https://whatever.example/ignored",
		"exp":  now.Add(time.Minute).Unix(),
		"body": bodyHash(body),
	})

	v := newTestVerifier(t, key, "", now)
	// Pass empty expectedURL — URL check is skipped.
	if err := v.Verify(jwt, body, ""); err != nil {
		t.Fatalf("expected ok when URL skipped, got %v", err)
	}
}
