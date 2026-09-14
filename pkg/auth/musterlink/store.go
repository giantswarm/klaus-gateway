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

// Store persists Slack-user -> muster Link associations. Backends only
// report: Get returns ErrNotLinked when no link is stored for the user and any
// other error when the backend failed to answer (a Kubernetes API call that did
// not go through, a bolt file that cannot be read), and Put and Delete return
// the backend's error. What a failure means -- a link served from memory, a
// write retried, a transient error to the person -- is the Linker's call. The
// interface is kept narrow so backends (bolt file, Kubernetes Secret,
// in-memory) are interchangeable without touching callers.
type Store interface {
	Get(slackUserID string) (*Link, error)
	Put(slackUserID string, link *Link) error
	Delete(slackUserID string) error
}

// MemStore is an in-memory Store. It loses all links on restart, forcing every
// user to re-link; use it only for tests and ephemeral single-process runs.
type MemStore struct {
	mu sync.RWMutex
	m  map[string]Link
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{m: map[string]Link{}} }

// Get returns a copy of the stored link, or ErrNotLinked when absent.
func (s *MemStore) Get(slackUserID string) (*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.m[slackUserID]
	if !ok {
		return nil, ErrNotLinked
	}
	return &l, nil
}

// Put upserts a copy of link.
func (s *MemStore) Put(slackUserID string, link *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[slackUserID] = *link
	return nil
}

// Delete removes a link; missing keys are a no-op.
func (s *MemStore) Delete(slackUserID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, slackUserID)
	return nil
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
// A store opened this way serves Get and Each; Put and Delete return bolt's
// read-only error. It is the import's way of reading a file it must leave
// untouched.
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

// Get decrypts and returns the link for slackUserID, ErrNotLinked when absent,
// and bolt's error when the file could not be read. A record that does not
// decode (written under another key, or corrupt) is logged and reported as
// ErrNotLinked: a re-link overwrites it, which heals it.
func (s *BoltStore) Get(slackUserID string) (*Link, error) {
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
		return nil, fmt.Errorf("musterlink: bolt read: %w", err)
	}
	if record == nil {
		return nil, ErrNotLinked
	}
	link, err := s.cipher.openLink(record)
	if err != nil {
		s.logger.Error("musterlink: decode link failed, treating the user as unlinked", "err", err)
		return nil, ErrNotLinked
	}
	return link, nil
}

// Put encrypts and stores link. It returns bolt's error when the write did not
// go through (a full disk, a read-only store): the file then still holds the
// previous record.
func (s *BoltStore) Put(slackUserID string, link *Link) error {
	record, err := s.cipher.sealLink(link)
	if err != nil {
		return err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(linkBucket).Put([]byte(slackUserID), record)
	}); err != nil {
		return fmt.Errorf("musterlink: bolt write: %w", err)
	}
	return nil
}

// Delete removes a link; missing keys are a no-op. It returns bolt's error
// when the write did not go through.
func (s *BoltStore) Delete(slackUserID string) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(linkBucket).Delete([]byte(slackUserID))
	}); err != nil {
		return fmt.Errorf("musterlink: bolt delete: %w", err)
	}
	return nil
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
