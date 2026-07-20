package musterlink

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Link is the persisted association between a Slack user and a muster identity.
// RefreshToken is the long-lived (rotating) muster refresh token used to
// silently mint fresh human muster tokens; it is the secret this package
// encrypts at rest.
type Link struct {
	Sub          string    `json:"sub"`
	Email        string    `json:"email"`
	RefreshToken string    `json:"refresh_token"`
	LinkedAt     time.Time `json:"linked_at"`
	// IDToken caches the last dex id_token obtained for this user so the gateway
	// reuses it across messages instead of refreshing on every call. muster
	// rotates refresh tokens, so refreshing per message (and Slack event retries
	// make that several times per turn) races the rotation: the second refresh
	// reuses an already-rotated token, fails invalid_grant, and burns the link.
	// Caching until expiry collapses a turn to at most one refresh.
	IDToken string `json:"id_token,omitempty"`
	// Expiry is when IDToken expires. Zero means unknown -> always refresh.
	Expiry time.Time `json:"expiry,omitzero"`
}

// Store persists Slack-user -> muster Link associations. The interface is
// intentionally error-free so the per-message Slack dispatch path stays simple;
// backends surface failures through their injected logger and degrade to a
// cache miss (Get -> false), which the caller treats as "not linked" and
// re-prompts. The interface is kept narrow so the bolt backend can later be
// swapped for Valkey or a Kubernetes Secret without touching callers.
type Store interface {
	Get(slackUserID string) (*Link, bool)
	Put(slackUserID string, link *Link)
	Delete(slackUserID string)
}

// MemStore is an in-memory Store. It loses all links on restart, forcing every
// user to re-link; use it only for tests and ephemeral single-process runs.
type MemStore struct {
	mu sync.RWMutex
	m  map[string]Link
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{m: map[string]Link{}} }

// Get returns a copy of the stored link, or (nil, false) when absent.
func (s *MemStore) Get(slackUserID string) (*Link, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.m[slackUserID]
	if !ok {
		return nil, false
	}
	return &l, true
}

// Put upserts a copy of link.
func (s *MemStore) Put(slackUserID string, link *Link) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[slackUserID] = *link
}

// Delete removes a link; missing keys are a no-op.
func (s *MemStore) Delete(slackUserID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, slackUserID)
}

var linkBucket = []byte("musterlinks")

// BoltStore is a bbolt-backed Store that encrypts each Link with AES-256-GCM
// before writing it to disk, so a leaked database file does not leak refresh
// tokens. The encryption key comes from a mounted secret.
//
// ponytail: single bolt file, no horizontal sharing. A multi-replica gateway
// needs a shared backend (Valkey / Secret); the Store interface is the seam.
type BoltStore struct {
	db     *bolt.DB
	gcm    cipher.AEAD
	logger *slog.Logger
}

// OpenBoltStore opens or creates an encrypted link store at path. key must
// resolve to a 32-byte AES-256 key: it is used verbatim when it is exactly 32
// raw bytes, otherwise it is base64- or hex-decoded (see normalizeStoreKey).
// A nil logger defaults to slog.Default().
func OpenBoltStore(path string, key []byte, logger *slog.Logger) (*BoltStore, error) {
	key, err := normalizeStoreKey(key)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("musterlink: open bolt %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(linkBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("musterlink: create bucket: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &BoltStore{db: db, gcm: gcm, logger: logger}, nil
}

// Get decrypts and returns the link for slackUserID, or (nil, false) when
// absent or on any decode/decrypt error (logged).
func (s *BoltStore) Get(slackUserID string) (*Link, bool) {
	var ciphertext []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(linkBucket).Get([]byte(slackUserID)); v != nil {
			ciphertext = append([]byte(nil), v...)
		}
		return nil
	})
	if err != nil {
		s.logger.Error("musterlink: bolt read failed", "err", err)
		return nil, false
	}
	if ciphertext == nil {
		return nil, false
	}
	plaintext, err := s.open(ciphertext)
	if err != nil {
		s.logger.Error("musterlink: decrypt link failed", "err", err)
		return nil, false
	}
	var l Link
	if err := json.Unmarshal(plaintext, &l); err != nil {
		s.logger.Error("musterlink: unmarshal link failed", "err", err)
		return nil, false
	}
	return &l, true
}

// Put encrypts and stores link. Errors are logged; a failed Put means the next
// refresh sees the stale token and the user re-links.
func (s *BoltStore) Put(slackUserID string, link *Link) {
	// G117: the marshaled link (incl. the refresh token) is encrypted with
	// AES-256-GCM by seal before it is ever written to disk.
	plaintext, err := json.Marshal(link) //nolint:gosec
	if err != nil {
		s.logger.Error("musterlink: marshal link failed", "err", err)
		return
	}
	ciphertext, err := s.seal(plaintext)
	if err != nil {
		s.logger.Error("musterlink: encrypt link failed", "err", err)
		return
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(linkBucket).Put([]byte(slackUserID), ciphertext)
	}); err != nil {
		s.logger.Error("musterlink: bolt write failed", "err", err)
	}
}

// Delete removes a link; missing keys are a no-op. Errors are logged.
func (s *BoltStore) Delete(slackUserID string) {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(linkBucket).Delete([]byte(slackUserID))
	}); err != nil {
		s.logger.Error("musterlink: bolt delete failed", "err", err)
	}
}

// Close closes the underlying database.
func (s *BoltStore) Close() error { return s.db.Close() }

func (s *BoltStore) seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (s *BoltStore) open(ciphertext []byte) ([]byte, error) {
	ns := s.gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, fmt.Errorf("ciphertext shorter than nonce")
	}
	return s.gcm.Open(nil, ciphertext[:ns], ciphertext[ns:], nil)
}

// normalizeStoreKey resolves the configured link-store key to the raw 32-byte
// AES-256 key. A 32-byte input is raw key material and used as-is. Anything else
// is treated as a text encoding: surrounding whitespace is trimmed (secret files
// routinely carry a trailing newline) and the value is base64- or hex-decoded.
// Only a result of exactly 32 bytes is accepted, so a misconfigured key fails
// loudly at startup instead of silently weakening encryption. This is what makes
// a SOPS-staged 44-char base64 key (the common case) work without forcing
// operators to stage raw bytes.
func normalizeStoreKey(raw []byte) ([]byte, error) {
	if len(raw) == 32 {
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
		if k, err := decode(s); err == nil && len(k) == 32 {
			return k, nil
		}
	}
	return nil, fmt.Errorf("musterlink: store key must be 32 raw bytes or a base64/hex encoding of 32 bytes (got %d bytes)", len(raw))
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("musterlink: encryption key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("musterlink: new cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
