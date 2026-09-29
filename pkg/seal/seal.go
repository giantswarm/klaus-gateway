// Package seal encrypts small secrets the gateway keeps at rest with
// AES-256-GCM. A sealed record is nonce || ciphertext, and the caller's
// additional data binds it to where it is stored: a record copied under
// another key does not open.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the AES-256 key length.
const KeySize = 32

// NormalizeKey resolves a configured key to the raw 32-byte AES-256 key. A
// 32-byte input is raw key material and used as-is. Anything else is treated
// as a text encoding: surrounding whitespace is trimmed (secret files
// routinely carry a trailing newline) and the value is base64- or
// hex-decoded. Only a result of exactly 32 bytes is accepted, so a
// misconfigured key fails loudly at startup instead of silently weakening
// encryption.
func NormalizeKey(raw []byte) ([]byte, error) {
	if len(raw) == KeySize {
		return raw, nil
	}
	s := strings.TrimSpace(string(raw))
	for _, decode := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	} {
		if k, err := decode(s); err == nil && len(k) == KeySize {
			return k, nil
		}
	}
	return nil, fmt.Errorf("seal: key must be %d raw bytes or a base64/hex encoding of %d bytes (got %d bytes)", KeySize, KeySize, len(raw))
}

// NewAEAD builds the AES-256-GCM AEAD for a raw 32-byte key.
func NewAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("seal: encryption key must be %d bytes (AES-256), got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("seal: new cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// Sealer seals and opens records under one key.
type Sealer struct {
	aead cipher.AEAD
}

// Derive returns a Sealer whose key is derived from the configured key with
// HKDF-SHA256 under info, so one configured secret serves several purposes
// without two of them sharing a key.
func Derive(configured []byte, info string) (*Sealer, error) {
	key, err := NormalizeKey(configured)
	if err != nil {
		return nil, err
	}
	sub, err := hkdf.Key(sha256.New, key, nil, info, KeySize)
	if err != nil {
		return nil, fmt.Errorf("seal: derive key: %w", err)
	}
	return newSealer(sub)
}

// Ephemeral returns a Sealer under a random key: its records open only in
// this process.
func Ephemeral() (*Sealer, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("seal: random key: %w", err)
	}
	return newSealer(key)
}

func newSealer(key []byte) (*Sealer, error) {
	aead, err := NewAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext bound to aad.
func (s *Sealer) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a record Seal wrote under the same aad.
func (s *Sealer) Open(record, aad []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(record) < ns {
		return nil, errors.New("seal: record shorter than nonce")
	}
	plaintext, err := s.aead.Open(nil, record[:ns], record[ns:], aad)
	if err != nil {
		return nil, fmt.Errorf("seal: decrypt: %w", err)
	}
	return plaintext, nil
}
