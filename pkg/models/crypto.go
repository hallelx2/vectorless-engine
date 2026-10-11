package models

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// sealVersion prefixes every sealed value so the format can change
// without guessing what an old row holds.
const sealVersion byte = 1

// ErrNoEncryptionKey is returned when a setting must be sealed or opened
// and the instance has no encryption key. Provider keys are never stored
// in the clear: without a key, keys can come only from the environment.
var ErrNoEncryptionKey = errors.New("models: no instance encryption key configured (set VLE_ENCRYPTION_KEY)")

// ErrSealedCorrupt is returned when a sealed value cannot be opened —
// a different encryption key, or a damaged row.
var ErrSealedCorrupt = errors.New("models: stored settings cannot be decrypted with this instance's encryption key")

// Cipher seals and opens instance settings with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from the instance encryption key.
//
// The key may be 32 raw bytes written as base64 (standard or URL, padded
// or not) or as 64 hex characters — what `openssl rand -base64 32` or
// `-hex 32` print. Any other string of at least 32 characters is
// accepted as a passphrase and stretched with SHA-256, so an operator's
// existing long random secret works unchanged. Shorter strings are
// refused: they are too weak to protect provider keys.
func NewCipher(key string) (*Cipher, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, ErrNoEncryptionKey
	}
	raw, err := decodeKey(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("models: encryption key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("models: encryption key: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

func decodeKey(key string) ([]byte, error) {
	if len(key) == 64 {
		if b, err := hex.DecodeString(key); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(key); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("models: encryption key too short: want 32 bytes as base64/hex, or a passphrase of at least 32 characters (got %d characters)", len(key))
	}
	sum := sha256.Sum256([]byte(key))
	return sum[:], nil
}

// Seal encrypts plaintext. aad binds the ciphertext to its purpose (the
// setting's key), so a sealed value cannot be moved to another row.
func (c *Cipher) Seal(plaintext []byte, aad string) ([]byte, error) {
	if c == nil {
		return nil, ErrNoEncryptionKey
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("models: nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	out = append(out, sealVersion)
	out = append(out, nonce...)
	return c.aead.Seal(out, nonce, plaintext, []byte(aad)), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (c *Cipher) Open(sealed []byte, aad string) ([]byte, error) {
	if c == nil {
		return nil, ErrNoEncryptionKey
	}
	ns := c.aead.NonceSize()
	if len(sealed) < 1+ns+c.aead.Overhead() || sealed[0] != sealVersion {
		return nil, ErrSealedCorrupt
	}
	nonce := sealed[1 : 1+ns]
	pt, err := c.aead.Open(nil, nonce, sealed[1+ns:], []byte(aad))
	if err != nil {
		return nil, ErrSealedCorrupt
	}
	return pt, nil
}
