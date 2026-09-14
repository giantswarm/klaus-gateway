package musterlink

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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
// re-prompts. The interface is kept narrow so backends (bolt file, Kubernetes
// Secret, in-memory) are interchangeable without touching callers.
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

// linkCipher turns a Link into an AES-256-GCM sealed record and back. Both
// persistent backends share it, so a record the bolt store wrote decrypts in
// the Secret store under the same key -- the one-time import relies on that.
type linkCipher struct {
	gcm cipher.AEAD
}

// newLinkCipher resolves key with normalizeStoreKey and builds the AEAD.
func newLinkCipher(key []byte) (*linkCipher, error) {
	key, err := normalizeStoreKey(key)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &linkCipher{gcm: gcm}, nil
}

// sealLink marshals and encrypts link; the record is nonce || ciphertext.
func (c *linkCipher) sealLink(link *Link) ([]byte, error) {
	// G117: the marshaled link (incl. the refresh token) is encrypted with
	// AES-256-GCM before it is ever handed to a backend.
	plaintext, err := json.Marshal(link) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("marshal link: %w", err)
	}
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return c.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// openLink decrypts and unmarshals a record written by sealLink.
func (c *linkCipher) openLink(record []byte) (*Link, error) {
	ns := c.gcm.NonceSize()
	if len(record) < ns {
		return nil, errors.New("record shorter than nonce")
	}
	plaintext, err := c.gcm.Open(nil, record[:ns], record[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt link: %w", err)
	}
	var l Link
	if err := json.Unmarshal(plaintext, &l); err != nil {
		return nil, fmt.Errorf("unmarshal link: %w", err)
	}
	return &l, nil
}

var linkBucket = []byte("musterlinks")

// BoltStore is a bbolt-backed Store that encrypts each Link with AES-256-GCM
// before writing it to disk, so a leaked database file does not leak refresh
// tokens. The encryption key comes from a mounted secret.
//
// A bolt file is node-bound (one ReadWriteOnce volume, one pod): a gateway
// that must survive node loss or run more than one replica uses SecretStore.
type BoltStore struct {
	db     *bolt.DB
	cipher *linkCipher
	logger *slog.Logger
}

// OpenBoltStore opens or creates an encrypted link store at path. key must
// resolve to a 32-byte AES-256 key: it is used verbatim when it is exactly 32
// raw bytes, otherwise it is base64- or hex-decoded (see normalizeStoreKey).
// A nil logger defaults to slog.Default().
func OpenBoltStore(path string, key []byte, logger *slog.Logger) (*BoltStore, error) {
	return openBoltStore(path, key, logger, false)
}

// OpenBoltStoreReadOnly opens an existing link store without taking bolt's
// write lock and without writing to the file, not even to create the bucket.
// A store opened this way serves Get and Each; Put and Delete fail and are
// logged. It is the import's way of reading a file it must leave untouched.
func OpenBoltStoreReadOnly(path string, key []byte, logger *slog.Logger) (*BoltStore, error) {
	return openBoltStore(path, key, logger, true)
}

func openBoltStore(path string, key []byte, logger *slog.Logger, readOnly bool) (*BoltStore, error) {
	c, err := newLinkCipher(key)
	if err != nil {
		return nil, err
	}
	if readOnly {
		// bolt refuses to open a missing file read-only only after creating it;
		// check first so a read-only open never leaves an empty database behind.
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("musterlink: open bolt %s: %w", path, err)
		}
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second, ReadOnly: readOnly})
	if err != nil {
		return nil, fmt.Errorf("musterlink: open bolt %s: %w", path, err)
	}
	if !readOnly {
		if err := db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists(linkBucket)
			return err
		}); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("musterlink: create bucket: %w", err)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &BoltStore{db: db, cipher: c, logger: logger}, nil
}

// Get decrypts and returns the link for slackUserID, or (nil, false) when
// absent or on any decode/decrypt error (logged).
func (s *BoltStore) Get(slackUserID string) (*Link, bool) {
	var record []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(linkBucket)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(slackUserID)); v != nil {
			record = append([]byte(nil), v...)
		}
		return nil
	})
	if err != nil {
		s.logger.Error("musterlink: bolt read failed", "err", err)
		return nil, false
	}
	if record == nil {
		return nil, false
	}
	link, err := s.cipher.openLink(record)
	if err != nil {
		s.logger.Error("musterlink: decode link failed", "err", err)
		return nil, false
	}
	return link, true
}

// Put encrypts and stores link. Errors are logged; a failed Put means the next
// refresh sees the stale token and the user re-links.
func (s *BoltStore) Put(slackUserID string, link *Link) {
	record, err := s.cipher.sealLink(link)
	if err != nil {
		s.logger.Error("musterlink: encrypt link failed", "err", err)
		return
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(linkBucket).Put([]byte(slackUserID), record)
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

// Each calls fn for every link in the store. A record that does not decrypt
// (written under another key, or corrupt) is logged and skipped so one bad
// entry never blocks an import. fn's error stops the iteration and is returned.
func (s *BoltStore) Each(fn func(slackUserID string, link *Link) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(linkBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			link, err := s.cipher.openLink(v)
			if err != nil {
				s.logger.Error("musterlink: skipping undecodable link record", "slackUser", string(k), "err", err)
				return nil
			}
			return fn(string(k), link)
		})
	})
}

// Close closes the underlying database.
func (s *BoltStore) Close() error { return s.db.Close() }

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
