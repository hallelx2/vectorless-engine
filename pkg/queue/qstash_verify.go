package queue

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// QStash signs webhook deliveries with a JWT (HS256) placed in the
// `Upstash-Signature` header. The canonical reference is Upstash's Node
// SDK (`@upstash/qstash`'s `Receiver.verify`) and the public docs at
// https://upstash.com/docs/qstash/features/security.
//
// The JWT payload contains:
//
//	iss   Upstash (constant)
//	sub   The destination URL QStash called (e.g. the engine webhook)
//	exp   Expiry (unix seconds)
//	nbf   Not-before (unix seconds)
//	iat   Issued-at
//	jti   JWT ID
//	body  base64url(sha256(body)), no padding
//
// Two signing keys are supported at once — current and next — to allow
// zero-downtime rotation. Verification succeeds if either HMAC matches.

// Verifier verifies QStash webhook signatures.
//
// Construct once at startup and reuse; the zero value is not usable.
type Verifier struct {
	currentKey []byte
	nextKey    []byte

	// clockSkew is the tolerated drift when checking exp/nbf. The Upstash
	// SDK uses 0 by default; we match.
	clockSkew time.Duration

	// now is injectable for tests. nil means time.Now.
	now func() time.Time
}

// VerifierConfig bundles Verifier construction params.
type VerifierConfig struct {
	// CurrentSigningKey is the active QStash signing key. Required.
	CurrentSigningKey string
	// NextSigningKey is the rotating-next key. Optional; set when rotating.
	NextSigningKey string
	// ClockSkew is the tolerated drift on exp/nbf. Zero = strict.
	ClockSkew time.Duration
}

// NewVerifier returns a Verifier, or an error if no signing key is set.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if cfg.CurrentSigningKey == "" {
		return nil, errors.New("qstash verifier: current_signing_key is required")
	}
	return &Verifier{
		currentKey: []byte(cfg.CurrentSigningKey),
		nextKey:    []byte(cfg.NextSigningKey),
		clockSkew:  cfg.ClockSkew,
	}, nil
}

// Verify checks the signature against the request body and expected URL.
//
// signature is the raw `Upstash-Signature` header value (a compact JWT).
// body is the raw request body bytes.
// expectedURL is the full URL QStash was told to call — it is matched
// against the JWT's `sub` claim. If expectedURL is empty, the URL check
// is skipped (useful behind reverse proxies where reconstructing the URL
// reliably is tricky, at the cost of losing one defense-in-depth layer).
func (v *Verifier) Verify(signature string, body []byte, expectedURL string) error {
	if v == nil {
		return errors.New("qstash verifier: nil receiver")
	}
	if signature == "" {
		return errors.New("qstash verifier: empty signature")
	}

	parts := strings.Split(signature, ".")
	if len(parts) != 3 {
		return errors.New("qstash verifier: malformed jwt")
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]
	signingInput := headerB64 + "." + payloadB64

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("qstash verifier: decode signature: %w", err)
	}

	// Try current key, then next key (rotation support). Constant-time
	// compares guard against timing attacks.
	ok := hmacMatches(v.currentKey, signingInput, sig)
	if !ok && len(v.nextKey) > 0 {
		ok = hmacMatches(v.nextKey, signingInput, sig)
	}
	if !ok {
		return errors.New("qstash verifier: signature mismatch")
	}

	// Parse header — we only accept HS256/JWT.
	headerJSON, err := base64.RawURLEncoding.DecodeString(headerB64)
	if err != nil {
		return fmt.Errorf("qstash verifier: decode header: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return fmt.Errorf("qstash verifier: parse header: %w", err)
	}
	if hdr.Alg != "HS256" {
		return fmt.Errorf("qstash verifier: unsupported alg %q", hdr.Alg)
	}

	// Parse payload.
	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return fmt.Errorf("qstash verifier: decode payload: %w", err)
	}
	var claims struct {
		Iss  string `json:"iss"`
		Sub  string `json:"sub"`
		Exp  int64  `json:"exp"`
		Nbf  int64  `json:"nbf"`
		Iat  int64  `json:"iat"`
		Jti  string `json:"jti"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return fmt.Errorf("qstash verifier: parse payload: %w", err)
	}

	now := v.nowFn()

	// Expiry. Upstash issues short-lived tokens; allow ClockSkew drift.
	if claims.Exp > 0 && now.Add(-v.clockSkew).After(time.Unix(claims.Exp, 0)) {
		return errors.New("qstash verifier: token expired")
	}
	if claims.Nbf > 0 && now.Add(v.clockSkew).Before(time.Unix(claims.Nbf, 0)) {
		return errors.New("qstash verifier: token not yet valid")
	}

	// URL binding: sub must match the URL we were called on, if supplied.
	// This stops an attacker from replaying a signed webhook intended for
	// a different endpoint. A bare string compare is fine — QStash echoes
	// the same URL we handed to the publish endpoint.
	if expectedURL != "" && claims.Sub != expectedURL {
		return fmt.Errorf("qstash verifier: sub %q does not match expected %q", claims.Sub, expectedURL)
	}

	// Body binding: the `body` claim is base64url(sha256(raw_body)).
	sum := sha256.Sum256(body)
	wantBody := base64.RawURLEncoding.EncodeToString(sum[:])
	// Be permissive about padding: some implementations use StdEncoding.
	gotBody := strings.TrimRight(claims.Body, "=")
	if gotBody != wantBody {
		return errors.New("qstash verifier: body hash mismatch")
	}

	return nil
}

func (v *Verifier) nowFn() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

func hmacMatches(key []byte, signingInput string, sig []byte) bool {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	return hmac.Equal(mac.Sum(nil), sig)
}
