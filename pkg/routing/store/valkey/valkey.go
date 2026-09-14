// Package valkey provides a Valkey-backed implementation of store.Store: one
// key per routing entry, the JSON-encoded entry as its value, the entry's TTL
// as the key's expiry. It is the store installations run: the platform already
// operates a Valkey for muster's token store, so the routing table shares it
// and survives a gateway restart without a volume or API-server access.
//
// Every operation is bounded by Options.Timeout (dial and command alike), so a
// Valkey outage fails the turn within seconds instead of hanging the thread.
// The connection is opened on first use and re-established by the client on
// its own once the server is back; a store that could not connect is retried
// on the next call, never left broken.
package valkey

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

const (
	// DefaultKeyPrefix is prepended to every routing key. The serialised
	// routing key starts with the channel, so one SCAN by channel prefix lists
	// a channel's entries.
	DefaultKeyPrefix = "klaus-gateway:route:"
	// DefaultTimeout bounds the dial and every command.
	DefaultTimeout = 2 * time.Second

	scanCount = 256
	mgetBatch = 128
	minExpiry = time.Millisecond
)

// Options configure the Valkey-backed store.
type Options struct {
	// URL is the server address as host:port.
	URL string
	// Username and Password authenticate the connection (ACL AUTH). An empty
	// Username means the server's default user.
	Username string
	Password string
	// DB is the logical database to SELECT.
	DB int
	// TLS enables TLS with the given configuration; nil keeps plaintext.
	TLS *tls.Config
	// KeyPrefix namespaces the keys; DefaultKeyPrefix when empty.
	KeyPrefix string
	// Timeout bounds the dial and every command; DefaultTimeout when zero.
	Timeout time.Duration
}

// Store persists routing entries in Valkey.
type Store struct {
	opts Options
	now  func() time.Time

	mu     sync.Mutex
	client valkeygo.Client
	closed bool
}

// New returns a Store for opts. Nothing is dialed here: the first operation
// connects, so a server that is down at start fails that operation (and the
// readiness probe) rather than the process.
func New(opts Options) (*Store, error) {
	if opts.URL == "" {
		return nil, errors.New("valkey: url is required")
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = DefaultKeyPrefix
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	return &Store{opts: opts, now: time.Now}, nil
}

// Get returns the entry for k, or (_, false, nil) when absent or expired.
func (s *Store) Get(ctx context.Context, k store.Key) (store.Entry, bool, error) {
	c, err := s.conn()
	if err != nil {
		return store.Entry{}, false, err
	}
	raw, err := s.do(ctx, c, c.B().Get().Key(s.key(k)).Build()).AsBytes()
	if valkeygo.IsValkeyNil(err) {
		return store.Entry{}, false, nil
	}
	if err != nil {
		return store.Entry{}, false, fmt.Errorf("valkey: get: %w", err)
	}
	var e store.Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return store.Entry{}, false, fmt.Errorf("valkey: decode entry: %w", err)
	}
	if e.Expired(s.now()) {
		return store.Entry{}, false, nil
	}
	return e, true, nil
}

// Put upserts an entry. An entry with a TTL expires server-side after what is
// left of it (TTL counted from LastSeen, the contract Entry.Expired states);
// an entry that has already expired removes the key instead.
func (s *Store) Put(ctx context.Context, k store.Key, e store.Entry) error {
	now := s.now()
	if e.Expired(now) {
		return s.Delete(ctx, k)
	}
	buf, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("valkey: encode entry: %w", err)
	}
	c, err := s.conn()
	if err != nil {
		return err
	}
	set := c.B().Set().Key(s.key(k)).Value(string(buf))
	var cmd valkeygo.Completed
	if e.TTL > 0 {
		cmd = set.Px(remaining(e, now)).Build()
	} else {
		cmd = set.Build()
	}
	if err := s.do(ctx, c, cmd).Error(); err != nil {
		return fmt.Errorf("valkey: set: %w", err)
	}
	return nil
}

// Delete removes an entry; a missing key is not an error.
func (s *Store) Delete(ctx context.Context, k store.Key) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	if err := s.do(ctx, c, c.B().Del().Key(s.key(k)).Build()).Error(); err != nil {
		return fmt.Errorf("valkey: del: %w", err)
	}
	return nil
}

// List returns every live entry under the key prefix: a SCAN by prefix, then
// the values in batches. A key under the prefix that is not a routing key is
// reported as an error, since the namespace is this store's alone.
func (s *Store) List(ctx context.Context) ([]store.KeyEntry, error) {
	c, err := s.conn()
	if err != nil {
		return nil, err
	}
	keys, err := s.scan(ctx, c)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]store.KeyEntry, 0, len(keys))
	for start := 0; start < len(keys); start += mgetBatch {
		batch := keys[start:min(start+mgetBatch, len(keys))]
		values, err := s.do(ctx, c, c.B().Mget().Key(batch...).Build()).ToArray()
		if err != nil {
			return nil, fmt.Errorf("valkey: mget: %w", err)
		}
		for i, v := range values {
			if v.IsNil() {
				continue // expired or deleted between the SCAN and the MGET
			}
			raw, err := v.AsBytes()
			if err != nil {
				return nil, fmt.Errorf("valkey: mget %s: %w", batch[i], err)
			}
			var e store.Entry
			if err := json.Unmarshal(raw, &e); err != nil {
				return nil, fmt.Errorf("valkey: decode entry %s: %w", batch[i], err)
			}
			if e.Expired(now) {
				continue
			}
			k, err := store.ParseKey(strings.TrimPrefix(batch[i], s.opts.KeyPrefix))
			if err != nil {
				return nil, fmt.Errorf("valkey: key %s under the routing prefix: %w", batch[i], err)
			}
			out = append(out, store.KeyEntry{Key: k, Entry: e})
		}
	}
	return out, nil
}

// Ping checks the connection; the readiness probe uses it instead of List.
func (s *Store) Ping(ctx context.Context) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	if err := s.do(ctx, c, c.B().Ping().Build()).Error(); err != nil {
		return fmt.Errorf("valkey: ping: %w", err)
	}
	return nil
}

// Close releases the connection. Further calls fail.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	return nil
}

// SetNowFunc is a test hook to override the clock.
func (s *Store) SetNowFunc(f func() time.Time) { s.now = f }

func (s *Store) key(k store.Key) string { return s.opts.KeyPrefix + k.String() }

// conn returns the client, dialing on first use. A failed dial is not
// remembered: the next call tries again.
func (s *Store) conn() (valkeygo.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("valkey: store is closed")
	}
	if s.client != nil {
		return s.client, nil
	}
	c, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress:      []string{s.opts.URL},
		Username:         s.opts.Username,
		Password:         s.opts.Password,
		SelectDB:         s.opts.DB,
		TLSConfig:        s.opts.TLS,
		ClientName:       "klaus-gateway",
		Dialer:           net.Dialer{Timeout: s.opts.Timeout},
		ConnWriteTimeout: s.opts.Timeout,
		// The store issues plain commands; no client-side cache, no cluster
		// probe, and no retry that would stretch an outage past the timeout.
		DisableCache:      true,
		DisableRetry:      true,
		ForceSingleClient: true,
	})
	if err != nil {
		return nil, fmt.Errorf("valkey: connect %s: %w", s.opts.URL, err)
	}
	s.client = c
	return c, nil
}

// do runs cmd under the store's timeout (tighter of ctx and Options.Timeout).
func (s *Store) do(ctx context.Context, c valkeygo.Client, cmd valkeygo.Completed) valkeygo.ValkeyResult {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	return c.Do(ctx, cmd)
}

// scan collects the keys under the prefix. SCAN may repeat a key across
// iterations, so the result is deduplicated.
func (s *Store) scan(ctx context.Context, c valkeygo.Client) ([]string, error) {
	pattern := globEscape(s.opts.KeyPrefix) + "*"
	seen := make(map[string]struct{})
	var keys []string
	var cursor uint64
	for {
		page, err := s.do(ctx, c, c.B().Scan().Cursor(cursor).Match(pattern).Count(scanCount).Build()).AsScanEntry()
		if err != nil {
			return nil, fmt.Errorf("valkey: scan: %w", err)
		}
		for _, k := range page.Elements {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
		cursor = page.Cursor
		if cursor == 0 {
			return keys, nil
		}
	}
}

// remaining is what is left of e's TTL counted from LastSeen, clamped to
// [minExpiry, TTL] so a future LastSeen cannot extend it.
func remaining(e store.Entry, now time.Time) time.Duration {
	left := e.TTL - now.Sub(e.LastSeen)
	if left > e.TTL {
		left = e.TTL
	}
	if left < minExpiry {
		left = minExpiry
	}
	return left
}

// globEscape quotes the characters SCAN MATCH treats as glob syntax.
func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
