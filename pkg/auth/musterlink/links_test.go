package musterlink

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errStoreDown = errors.New("store: connection refused")

// flakyStore wraps a Store and fails the operations a test switches on, the
// way the Secret backend fails while the apiserver is away; it also counts
// reads so a test can tell copies served from the process from store reads.
type flakyStore struct {
	inner Store

	mu                           sync.Mutex
	failGet, failPut, failDelete bool
	gets                         int
}

func newFlakyStore(inner Store) *flakyStore { return &flakyStore{inner: inner} }

func (f *flakyStore) set(fn func(f *flakyStore)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *flakyStore) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

func (f *flakyStore) Get(id string) (*Link, error) {
	f.mu.Lock()
	f.gets++
	fail := f.failGet
	f.mu.Unlock()
	if fail {
		return nil, errStoreDown
	}
	return f.inner.Get(id)
}

func (f *flakyStore) Put(id string, link *Link) error {
	f.mu.Lock()
	fail := f.failPut
	f.mu.Unlock()
	if fail {
		return errStoreDown
	}
	return f.inner.Put(id, link)
}

func (f *flakyStore) Delete(id string) error {
	f.mu.Lock()
	fail := f.failDelete
	f.mu.Unlock()
	if fail {
		return errStoreDown
	}
	return f.inner.Delete(id)
}

// clock is a movable now for the linker: advancing it past the id_token's
// lifetime forces a refresh, past cacheTTL a store re-read.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now()} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// presented is the refresh token the stub saw most recently (asserted by its
// stub-issued name, never logged or printed).
func (s *musterStub) presented() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRefresh
}

func (s *musterStub) issued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counter
}

// newFlakyLinker is a linker on fs with a movable clock and millisecond write
// retries; its Close runs at cleanup.
func newFlakyLinker(t *testing.T, stub *musterStub, fs *flakyStore, slackEmail func(context.Context, string) (string, error)) (*Linker, *clock) {
	t.Helper()
	l := newTestLinker(t, stub, fs, slackEmail)
	clk := newClock()
	l.now = clk.now
	l.retryDelay, l.nextDelay = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { _ = l.Close() })
	return l, clk
}

func seeded(t *testing.T) (*MemStore, *flakyStore) {
	t.Helper()
	mem := NewMemStore()
	require.NoError(t, mem.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0"}))
	return mem, newFlakyStore(mem)
}

func storedRefresh(t *testing.T, s Store) string {
	t.Helper()
	got, err := s.Get("U1")
	require.NoError(t, err)
	return got.RefreshToken
}

// A store that cannot be read is not "nobody is linked": the caller gets the
// store's failure (a transient error), not ErrNotLinked -- the Slack adapter
// turns the former into the token-error notice and the latter into a sign-in
// prompt for a person who is signed in.
func TestTokenForStoreReadFailureIsTransientNotUnlinked(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	_, fs := seeded(t)
	l, _ := newFlakyLinker(t, stub, fs, nil)
	fs.set(func(f *flakyStore) { f.failGet = true })

	_, err := l.TokenFor(context.Background(), "U1")
	require.ErrorIs(t, err, errStoreDown, "the store's failure reaches the caller")
	require.NotErrorIs(t, err, ErrNotLinked)
	_, _, ok := l.LinkedIdentity("U1")
	require.False(t, ok)
	require.Equal(t, 0, stub.issued(), "nothing was spent at muster")

	// The store answers again: the link is there, no sign-in needed.
	fs.set(func(f *flakyStore) { f.failGet = false })
	tok, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "muster-sub", jwtSub(t, tok))
}

// A person this process has served keeps working while the store is down:
// the copy is served, and the failing store is asked once per cacheTTL rather
// than on every message.
func TestTokenForServesKnownLinkWhileStoreIsDown(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	_, fs := seeded(t)
	l, clk := newFlakyLinker(t, stub, fs, nil)

	tok1, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)

	fs.set(func(f *flakyStore) { f.failGet = true })
	clk.advance(l.cacheTTL + time.Second) // the copy is due for a re-check, which fails
	tok2, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err, "a person this process knows keeps working while the store is down")
	require.Equal(t, tok1, tok2)
	sub, email, ok := l.LinkedIdentity("U1")
	require.True(t, ok)
	require.Equal(t, "muster-sub", sub)
	require.Equal(t, "a@example.com", email)

	before := fs.reads()
	_, err = l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	_, _, _ = l.LinkedIdentity("U1")
	require.Equal(t, before, fs.reads(), "the failing store is not asked again before cacheTTL")
}

// The heart of the fix: a refresh token muster has rotated is never lost to a
// store that refuses the write. The turn proceeds, the next refresh spends the
// rotated token the store never saw (the spent one it holds would fail
// invalid_grant and sign the person out), the write lands when the store is
// back, and a restarted process finds the live token.
func TestTokenForKeepsRotatedTokenWhenStoreWriteFails(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	mem, fs := seeded(t)
	l, clk := newFlakyLinker(t, stub, fs, nil)
	fs.set(func(f *flakyStore) { f.failPut = true })

	tok, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err, "muster minted the token; only the store failed")
	require.Equal(t, "muster-sub", jwtSub(t, tok))
	require.Equal(t, "refresh-0", storedRefresh(t, mem), "the store still holds the spent token")

	// The id_token expires while the store is still down.
	clk.advance(2 * time.Hour)
	_, err = l.TokenFor(context.Background(), "U1")
	require.NoError(t, err, "the rotated token is refreshed, the link survives")
	require.Equal(t, "refresh-1", stub.presented(), "the rotated token was spent, not the spent one the store holds")
	require.Equal(t, "refresh-0", storedRefresh(t, mem))

	// The store takes writes again: the pending write lands with the newest
	// version, and a restart (a new process on the same store) refreshes
	// without invalid_grant.
	fs.set(func(f *flakyStore) { f.failPut = false })
	require.Equal(t, 0, l.Flush())
	require.Equal(t, "refresh-2", storedRefresh(t, mem))

	restarted := newTestLinker(t, stub, mem, nil)
	restarted.now = clk.now
	tok, err = restarted.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "muster-sub", jwtSub(t, tok))
	require.Equal(t, "refresh-2", stub.presented())
}

// A failed write is retried on its own: the store holds the rotated token
// shortly after it takes writes again, without another turn.
func TestTokenForRetriesFailedWriteInBackground(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	mem, fs := seeded(t)
	l, _ := newFlakyLinker(t, stub, fs, nil)
	fs.set(func(f *flakyStore) { f.failPut = true })

	_, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "refresh-0", storedRefresh(t, mem))

	fs.set(func(f *flakyStore) { f.failPut = false })
	require.Eventually(t, func() bool {
		got, err := mem.Get("U1")
		return err == nil && got.RefreshToken == "refresh-1"
	}, 2*time.Second, 5*time.Millisecond, "the background retry writes the rotated token")
}

// Close writes what the store has not taken yet, and says so when it cannot.
func TestCloseFlushesPendingWrites(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")

	mem, fs := seeded(t)
	l := newTestLinker(t, stub, fs, nil)
	fs.set(func(f *flakyStore) { f.failPut = true })
	_, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	fs.set(func(f *flakyStore) { f.failPut = false })
	require.NoError(t, l.Close(), "the shutdown flush lands the write")
	require.Equal(t, "refresh-1", storedRefresh(t, mem))

	// A second person on a second store, at a muster that has not seen their
	// seeded token yet.
	stub2 := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	mem2, fs2 := seeded(t)
	l2 := newTestLinker(t, stub2, fs2, nil)
	fs2.set(func(f *flakyStore) { f.failPut = true })
	_, err = l2.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	err = l2.Close()
	require.Error(t, err, "a store still down at shutdown is reported")
	require.Contains(t, err.Error(), "1 link")
	require.Equal(t, "refresh-0", storedRefresh(t, mem2))
}

// invalid_grant on the copy's refresh token is not yet a dead link: another
// writer (a second replica here) may have rotated it since this process last
// read the store. The store is re-read and the stored token retried; only when
// the store agrees is the link dropped (TestTokenForInvalidGrantDropsLink).
func TestTokenForInvalidGrantRereadsStoreBeforeDroppingLink(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	require.NoError(t, store.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0"}))

	a := newTestLinker(t, stub, store, nil)
	clkA := newClock()
	a.now = clkA.now
	a.cacheTTL = 24 * time.Hour // a's copy stays authoritative for the test: it never re-reads on its own
	_, err := a.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "refresh-1", storedRefresh(t, store))

	// Replica b serves the same person after the id_token expired: it spends
	// refresh-1 and writes refresh-2.
	b := newTestLinker(t, stub, store, nil)
	clkB := newClock()
	clkB.advance(2 * time.Hour)
	b.now = clkB.now
	_, err = b.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "refresh-1", stub.presented())
	require.Equal(t, "refresh-2", storedRefresh(t, store))

	// a's copy still says refresh-1, which muster now rejects. a must not burn
	// the link: it re-reads the store and retries with refresh-2.
	clkA.advance(2 * time.Hour)
	tok, err := a.TokenFor(context.Background(), "U1")
	require.NoError(t, err, "a token rotated by another replica is retried, not burned")
	require.Equal(t, "muster-sub", jwtSub(t, tok))
	require.Equal(t, "refresh-2", stub.presented())
	require.Equal(t, "refresh-3", storedRefresh(t, store))
}

// A sign-in whose link the store refuses to take still signs the person in
// here: the link is kept and written later, and their next turn runs without
// a second sign-in.
func TestCallbackKeepsLinkWhenStoreWriteFails(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "alice@example.com", "muster-sub")
	mem := NewMemStore()
	fs := newFlakyStore(mem)
	l, _ := newFlakyLinker(t, stub, fs, func(context.Context, string) (string, error) { return "alice@example.com", nil })
	fs.set(func(f *flakyStore) { f.failPut = true })

	driveCallback(t, l, "U1", http.StatusSeeOther)
	_, err := mem.Get("U1")
	require.ErrorIs(t, err, ErrNotLinked, "the store did not take the link")

	tok, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err, "the person is linked in this process right away")
	require.Equal(t, "muster-sub", jwtSub(t, tok))
	_, email, ok := l.LinkedIdentity("U1")
	require.True(t, ok)
	require.Equal(t, "alice@example.com", email)

	fs.set(func(f *flakyStore) { f.failPut = false })
	require.Eventually(t, func() bool {
		got, err := mem.Get("U1")
		return err == nil && got.Email == "alice@example.com"
	}, 2*time.Second, 5*time.Millisecond, "the link lands once the store is back")
}

// A sign-out the store refuses is reported, not confirmed: the store keeps the
// refresh token, and the person must know their /logout did not take.
func TestUnlinkReportsStoreFailure(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	mem, fs := seeded(t)
	l, _ := newFlakyLinker(t, stub, fs, nil)
	_, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)

	fs.set(func(f *flakyStore) { f.failDelete = true })
	require.ErrorIs(t, l.Unlink("U1"), errStoreDown)
	require.Equal(t, "refresh-1", storedRefresh(t, mem), "the store still holds the link")

	fs.set(func(f *flakyStore) { f.failDelete = false })
	require.NoError(t, l.Unlink("U1"))
	_, err = l.TokenFor(context.Background(), "U1")
	require.ErrorIs(t, err, ErrNotLinked)
}

// The lookups of one Slack turn (the token, the dispatch record's identity,
// the initiator's token) cost one store read, not one each.
func TestTokenForCollapsesStoreReadsWithinTTL(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	_, fs := seeded(t)
	l, clk := newFlakyLinker(t, stub, fs, nil)

	_, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	before := fs.reads()
	for range 5 {
		_, err := l.TokenFor(context.Background(), "U1")
		require.NoError(t, err)
		_, _, ok := l.LinkedIdentity("U1")
		require.True(t, ok)
	}
	require.Equal(t, before, fs.reads(), "a turn's lookups are served from the copy")

	clk.advance(l.cacheTTL + time.Second)
	_, err = l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, before+1, fs.reads(), "past cacheTTL the store is asked once more")

	// Absence is never cached: an unknown person is looked up on every call, so
	// a sign-in completed elsewhere is seen on their next message.
	before = fs.reads()
	_, err = l.TokenFor(context.Background(), "U2")
	require.ErrorIs(t, err, ErrNotLinked)
	after := fs.reads()
	require.Greater(t, after, before)
	_, err = l.TokenFor(context.Background(), "U2")
	require.ErrorIs(t, err, ErrNotLinked)
	require.Greater(t, fs.reads(), after)
}
