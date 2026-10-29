// Package seal keeps a stored credential unusable to whoever reads the database.
//
// burrow stores two kinds of secret it must be able to use again: a member's own
// MCP key, and an OAuth token. Neither can be hashed the way a session token is
// — burrow has to present both upstream — so both are sealed with AES-256-GCM
// under a key that comes from the environment (BURROW_MCP_CREDENTIAL_KEY, from
// OpenBao in deployment) and never from the database. A stolen dump is therefore
// a set of ciphertexts, and a server started without the key cannot decrypt,
// cannot seal, and so refuses to store a credential at all rather than writing
// one in the clear.
//
// Governing: ADR-0010 (Stage C: where a member's credential lives).
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNotConfigured marks the sealer that does not exist: no key was given, so
// nothing may be stored. Callers answer 503 with it rather than falling back to
// plaintext.
var ErrNotConfigured = errors.New("seal: no credential key configured")

// Sealer seals and opens credentials under one key. Build one with New.
type Sealer struct {
	aead    cipher.AEAD
	present bool
}

// New builds the sealer for a configured key. An empty key gives a nil *Sealer
// with no error: "not configured" is a supported configuration, not a broken one,
// and every method on a nil *Sealer refuses.
func New(key string) (*Sealer, error) {
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("seal: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("seal: new gcm: %w", err)
	}
	return &Sealer{aead: gcm, present: true}, nil
}

// Configured reports whether this sealer can seal anything.
func (s *Sealer) Configured() bool { return s != nil && s.present }

// Seal returns base64(nonce || ciphertext). An empty plaintext is empty out: a
// connection with no token in its refresh slot stores nothing, not a sealed
// empty string.
func (s *Sealer) Seal(plaintext string) (string, error) {
	if !s.Configured() {
		return "", ErrNotConfigured
	}
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("seal: nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

// Open reverses Seal. A value that was sealed under another key, or that was
// tampered with, is an error: the caller refuses the connection rather than
// presenting something it cannot trust.
func (s *Sealer) Open(sealed string) (string, error) {
	if !s.Configured() {
		return "", ErrNotConfigured
	}
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("seal: decode: %w", err)
	}
	size := s.aead.NonceSize()
	if len(raw) < size {
		return "", errors.New("seal: truncated")
	}
	plain, err := s.aead.Open(nil, raw[:size], raw[size:], nil)
	if err != nil {
		return "", fmt.Errorf("seal: open: %w", err)
	}
	return string(plain), nil
}

// Fingerprint is the credential's SHA-256 in hex: which credential a row holds,
// safe to log and enough to tell a re-save from a change, and never the value.
func Fingerprint(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("%x", sum[:])
}
