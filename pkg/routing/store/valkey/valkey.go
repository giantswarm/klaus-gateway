// Package valkey provides a Valkey-backed implementation of store.Store: one
// key per routing entry, the JSON-encoded entry as its value, the entry's TTL
// as the key's expiry; team reviews the same way under their own prefix, their
// updates a compare-and-set so replicas cannot both claim one review. It is
// the store installations run: the platform already operates a Valkey for
// muster's token store, so the routing table shares it and survives a gateway
// restart without a volume or API-server access.
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
	"hash/fnv"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

const (
	// DefaultKeyPrefix is prepended to every routing key. The serialised
	// routing key starts with the channel, so one SCAN by channel prefix lists
	// a channel's entries.
	DefaultKeyPrefix = "klaus-gateway:route:"
	// DefaultReviewKeyPrefix is prepended to every review id. It sits next to
	// the routing keys, not under them, so the routing SCAN never reads a
	// review.
	DefaultReviewKeyPrefix = "klaus-gateway:review:"
	// DefaultTimeout bounds the dial and every command.
	DefaultTimeout = 2 * time.Second

	scanCount = 256
	mgetBatch = 128
	minExpiry = time.Millisecond

	// routeSuffix is the last segment of the default routing prefix; the
	// review prefix is derived by swapping it (see ReviewKeyPrefix).
	routeSuffix  = "route:"
	reviewSuffix = "review:"
	// reviewUpdateAttempts bounds how often UpdateReview re-reads a record
	// another writer changed under it before giving up.
	reviewUpdateAttempts = 8
)

// reviewCAS writes ARGV[2] at KEYS[1] only while the key still holds ARGV[1],
// with ARGV[3] milliseconds of expiry (0 for none); it answers 1 when it
// wrote and 0 when the record changed or vanished meanwhile. Run as one
// script, the compare and the write cannot interleave with another client's.
const reviewCAS = `if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
if tonumber(ARGV[3]) > 0 then redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3]) else redis.call('SET', KEYS[1], ARGV[2]) end
return 1`

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
	// KeyPrefix namespaces the routing keys; DefaultKeyPrefix when empty.
	KeyPrefix string
	// ReviewKeyPrefix namespaces the review keys. Empty derives it from
	// KeyPrefix: a prefix ending in "route:" swaps that for "review:"
	// (DefaultReviewKeyPrefix for the default), any other has "review:"
	// appended — so a gateway that moves its routing keys moves its reviews
	// with them.
	ReviewKeyPrefix string
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

	// keyLocks serialise read-modify-write on one key inside this process.
	// Valkey has no CAS here, so a second replica is not protected.
	keyLocks [64]sync.Mutex
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
	if opts.ReviewKeyPrefix == "" {
		opts.ReviewKeyPrefix = strings.TrimSuffix(opts.KeyPrefix, routeSuffix) + reviewSuffix
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
	if err := s.set(ctx, c, s.key(k), buf, remaining(e.TTL, e.LastSeen, now)); err != nil {
		return fmt.Errorf("valkey: set: %w", err)
	}
	return nil
}

// set writes value at key, expiring after px when it is positive.
func (s *Store) set(ctx context.Context, c valkeygo.Client, key string, value []byte, px time.Duration) error {
	set := c.B().Set().Key(key).Value(string(value))
	var cmd valkeygo.Completed
	if px > 0 {
		cmd = set.Px(px).Build()
	} else {
		cmd = set.Build()
	}
	return s.do(ctx, c, cmd).Error()
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

// Update applies mutate to the entry at k and writes the result when mutate
// reports a change. The read-modify-write holds a per-key lock, so two writers
// of the same row inside this process cannot lose each other's fields; a
// second replica writing the same key is not protected.
func (s *Store) Update(ctx context.Context, k store.Key, mutate func(e *store.Entry, found bool) bool) error {
	mu := s.keyLock(k)
	mu.Lock()
	defer mu.Unlock()
	e, found, err := s.Get(ctx, k)
	if err != nil {
		return err
	}
	if !found {
		e = store.Entry{}
	}
	if !mutate(&e, found) {
		return nil
	}
	return s.Put(ctx, k, e)
}

// keyLock is the stripe serialising updates of k.
func (s *Store) keyLock(k store.Key) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(k.String()))
	return &s.keyLocks[h.Sum32()%uint32(len(s.keyLocks))]
}

// List returns every live entry under the key prefix: a SCAN by prefix, then
// the values in batches. A key under the prefix that no longer parses as a
// routing key is skipped: a key of an older layout outlives the upgrade until
// an operator removes it, and must not break the listing.
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
				continue
			}
			out = append(out, store.KeyEntry{Key: k, Entry: e})
		}
	}
	return out, nil
}

// PutReview upserts a review record under the review prefix, what is left of
// its TTL (counted from PostedAt) as the key's expiry; one that has already
// expired removes the key instead.
func (s *Store) PutReview(ctx context.Context, r store.Review) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	now := s.now()
	if r.Expired(now) {
		if err := s.do(ctx, c, c.B().Del().Key(s.reviewKey(r.ID)).Build()).Error(); err != nil {
			return fmt.Errorf("valkey: del review: %w", err)
		}
		return nil
	}
	buf, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("valkey: encode review: %w", err)
	}
	if err := s.set(ctx, c, s.reviewKey(r.ID), buf, remaining(r.TTL, r.PostedAt, now)); err != nil {
		return fmt.Errorf("valkey: set review: %w", err)
	}
	return nil
}

// GetReview returns the record for id, or (_, false, nil) when absent or expired.
func (s *Store) GetReview(ctx context.Context, id string) (store.Review, bool, error) {
	c, err := s.conn()
	if err != nil {
		return store.Review{}, false, err
	}
	_, r, found, err := s.readReview(ctx, c, s.reviewKey(id))
	return r, found, err
}

// UpdateReview applies mutate to the record at id and writes the result as a
// compare-and-set on the record's bytes (reviewCAS): a record another client
// changed between the read and the write is not overwritten but re-read, and
// mutate runs again on it. Replicas sharing the server therefore never lose
// each other's writes — two clicks cannot both claim one review.
func (s *Store) UpdateReview(ctx context.Context, id string, mutate func(r *store.Review) bool) (bool, error) {
	c, err := s.conn()
	if err != nil {
		return false, err
	}
	key := s.reviewKey(id)
	for range reviewUpdateAttempts {
		raw, r, found, err := s.readReview(ctx, c, key)
		if err != nil || !found {
			return false, err
		}
		if !mutate(&r) {
			return true, nil
		}
		buf, err := json.Marshal(r)
		if err != nil {
			return true, fmt.Errorf("valkey: encode review: %w", err)
		}
		px := remaining(r.TTL, r.PostedAt, s.now())
		cmd := c.B().Eval().Script(reviewCAS).Numkeys(1).Key(key).
			Arg(string(raw), string(buf), fmt.Sprint(px.Milliseconds())).Build()
		written, err := s.do(ctx, c, cmd).AsInt64()
		if err != nil {
			return true, fmt.Errorf("valkey: update review: %w", err)
		}
		if written == 1 {
			return true, nil
		}
	}
	return true, fmt.Errorf("valkey: review %s kept changing under %d update attempts", id, reviewUpdateAttempts)
}

// readReview returns the record at key with the bytes it was decoded from —
// the value the compare-and-set in UpdateReview compares against.
func (s *Store) readReview(ctx context.Context, c valkeygo.Client, key string) ([]byte, store.Review, bool, error) {
	raw, err := s.do(ctx, c, c.B().Get().Key(key).Build()).AsBytes()
	if valkeygo.IsValkeyNil(err) {
		return nil, store.Review{}, false, nil
	}
	if err != nil {
		return nil, store.Review{}, false, fmt.Errorf("valkey: get review: %w", err)
	}
	var r store.Review
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, store.Review{}, false, fmt.Errorf("valkey: decode review: %w", err)
	}
	if r.Expired(s.now()) {
		return nil, store.Review{}, false, nil
	}
	return raw, r, true, nil
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

func (s *Store) reviewKey(id string) string { return s.opts.ReviewKeyPrefix + id }

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
		DisableCache: true,
		DisableRetry: true,
		// One connection: PING and every keyed command share it, so the
		// readiness check proves the connection the next command uses, and a
		// connection the server closed is discovered and replaced by the first
		// failing command's retry (see do). The store sends a handful of small
		// commands per turn and needs no multiplexing.
		PipelineMultiplex: -1,
		ForceSingleClient: true,
	})
	if err != nil {
		return nil, fmt.Errorf("valkey: connect %s: %w", s.opts.URL, err)
	}
	s.client = c
	return c, nil
}

// do runs cmd under the store's timeout (tighter of ctx and Options.Timeout).
//
// A command that fails because its connection was closed is retried once,
// inside the same deadline: the client drops the dead connection on that
// failure and re-dials it on the retry, so after a Valkey restart or failover
// the first command no longer fails with EOF once the server is back
// (klaus-gateway#261; the store holds one connection, see conn). An outage
// still fails within the timeout, because the second attempt shares the first
// one's deadline. The client's own retry covers read-only commands only, so it
// stays disabled and this covers SET and DEL too.
func (s *Store) do(ctx context.Context, c valkeygo.Client, cmd valkeygo.Completed) valkeygo.ValkeyResult {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	// A guard, not a requirement: the client recycles a command's buffers
	// after a successful Do and keeps a failed one intact, so the retry would
	// be safe unpinned. Pinning makes the two sends independent of that detail.
	cmd = cmd.Pin()
	res := c.Do(ctx, cmd)
	if connectionClosed(res.Error()) && ctx.Err() == nil {
		res = c.Do(ctx, cmd)
	}
	return res
}

// connectionClosed reports whether err is the failure of a command whose
// connection went away — not a timeout, not a server reply.
func connectionClosed(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, valkeygo.ErrClosing)
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

// remaining is what is left of ttl counted from since, clamped to
// [minExpiry, ttl] so a future since cannot extend it. A ttl of zero or less
// means no expiry and stays 0.
func remaining(ttl time.Duration, since, now time.Time) time.Duration {
	if ttl <= 0 {
		return 0
	}
	left := ttl - now.Sub(since)
	if left > ttl {
		left = ttl
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
