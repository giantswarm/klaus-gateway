package musterlink

import (
	"context"
	"errors"
	"fmt"
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
	// conflicts is how many Update calls still answer errStoreConflict, as a
	// Secret store contended past its retries does.
	conflicts int
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

// Update fails like a read when reads fail and like a write when writes fail:
// it does both.
func (f *flakyStore) Update(id string, fn func(*Link) (*Link, bool)) error {
	f.mu.Lock()
	fail := f.failGet || f.failPut
	conflict := f.conflicts > 0
	if conflict {
		f.conflicts--
	}
	f.mu.Unlock()
	if fail {
		return errStoreDown
	}
	if conflict {
		return fmt.Errorf("secret: conflict retry limit exceeded: %w", errStoreConflict)
	}
	return f.inner.Update(id, fn)
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

// reuses counts the rotated refresh tokens presented again, each of which
// revoked its family.
func (s *musterStub) reuses() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reused
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

// A replica whose copy of the link is stale never presents the token another
// replica has rotated: muster would take that replay for theft and revoke the
// whole family, the rotated token included. The lease compares against the
// store first, so the stale replica adopts the stored link and refreshes with
// its token.
func TestTokenForAdoptsLinkRotatedByAnotherReplica(t *testing.T) {
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

	// Replica b serves the same person after the id_token expired: it presents
	// refresh-1 and writes refresh-2.
	b := newTestLinker(t, stub, store, nil)
	clkB := newClock()
	clkB.advance(2 * time.Hour)
	b.now = clkB.now
	_, err = b.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "refresh-1", stub.presented())
	require.Equal(t, "refresh-2", storedRefresh(t, store))

	// a's copy still says refresh-1. Presenting it would revoke the family.
	clkA.advance(2 * time.Hour)
	idToken, err := a.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "muster-sub", jwtSub(t, idToken))
	require.Equal(t, 0, stub.reuses(), "the rotated token was never presented again")
	require.Equal(t, "refresh-2", stub.presented())
	require.Equal(t, "refresh-3", storedRefresh(t, store))
}

// Two replicas whose copies expire together (both background refreshers
// sweep the same person, or two messages land on two pods) refresh once:
// the second finds the first one's lease, waits for its rotated link and
// serves its id_token.
func TestTokenForReplicasRefreshOnce(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	require.NoError(t, store.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0"}))
	entered, release := make(chan struct{}), make(chan struct{})
	var holdFirst, releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst) // runs before the stub's server closes, even when the test fails
	stub.mu.Lock()
	stub.onRefresh = func() {
		holdFirst.Do(func() {
			close(entered)
			<-release
		})
	}
	stub.mu.Unlock()

	a := newTestLinker(t, stub, store, nil)
	b := newTestLinker(t, stub, store, nil)
	b.leasePoll = time.Millisecond

	type result struct {
		idToken string
		err     error
	}
	fromA, fromB := make(chan result, 1), make(chan result, 1)
	go func() {
		idToken, err := a.TokenFor(context.Background(), "U1")
		fromA <- result{idToken, err}
	}()
	<-entered // a holds the lease and is at muster
	go func() {
		idToken, err := b.TokenFor(context.Background(), "U1")
		fromB <- result{idToken, err}
	}()
	require.Eventually(t, func() bool {
		got, err := store.Get("U1")
		return err == nil && got.Lease != nil && got.Lease.Holder == a.leaseHolder
	}, 2*time.Second, time.Millisecond)
	releaseFirst()

	resultA, resultB := <-fromA, <-fromB
	require.NoError(t, resultA.err)
	require.NoError(t, resultB.err)
	require.Equal(t, resultA.idToken, resultB.idToken, "b serves the id_token a obtained")
	require.Equal(t, 0, stub.reuses())
	require.Equal(t, 1, stub.issued(), "muster saw one refresh")
	got, err := store.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "refresh-1", got.RefreshToken)
	require.Nil(t, got.Lease, "the rotated link is written without the lease")
}

// A lease whose holder died mid-refresh expires: the token is refreshed by the
// next process once the lease has expired.
func TestTokenForTakesOverExpiredLease(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	require.NoError(t, store.Put("U1", &Link{
		Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0",
		Lease: &RefreshLease{Holder: "gone", Until: time.Now().Add(-time.Second)},
	}))
	l := newTestLinker(t, stub, store, nil)

	_, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, "refresh-0", stub.presented())
	require.Equal(t, "refresh-1", storedRefresh(t, store))
}

// A lease that holds past the wait fails the turn as transient and leaves
// the token alone: presenting it could revoke the family the holder is
// rotating.
func TestTokenForGivesUpOnLiveLease(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	require.NoError(t, store.Put("U1", &Link{
		Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0",
		Lease: &RefreshLease{Holder: "other", Until: time.Now().Add(time.Hour)},
	}))
	l := newTestLinker(t, stub, store, nil)
	l.leaseWait, l.leasePoll = 20*time.Millisecond, time.Millisecond

	_, err := l.TokenFor(context.Background(), "U1")
	require.ErrorIs(t, err, errRefreshContended)
	require.NotErrorIs(t, err, ErrNotLinked)
	require.Equal(t, 0, stub.issued(), "nothing was presented at muster")
	require.Equal(t, "refresh-0", storedRefresh(t, store))
}

// A lease write that stays contended is retried, never skipped: contention is
// other processes writing the link, the moment a refresh without a lease
// could present the same token as one of them. Once the store takes the lease
// the refresh runs under it.
func TestTokenForRetriesContendedLease(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	mem, fs := seeded(t)
	l, _ := newFlakyLinker(t, stub, fs, nil)
	l.leasePoll = time.Millisecond
	fs.set(func(f *flakyStore) { f.conflicts = 3 })
	var leased *RefreshLease
	stub.mu.Lock()
	stub.onRefresh = func() {
		if got, err := mem.Get("U1"); err == nil {
			leased = got.Lease
		}
	}
	stub.mu.Unlock()

	_, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.NotNil(t, leased, "the token was presented under a lease")
	require.Equal(t, l.leaseHolder, leased.Holder)
	require.Equal(t, "refresh-1", storedRefresh(t, mem))
}

// A store contended past the wait fails the turn as transient and presents
// nothing at muster.
func TestTokenForGivesUpOnContendedLease(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	mem, fs := seeded(t)
	l, _ := newFlakyLinker(t, stub, fs, nil)
	l.leaseWait, l.leasePoll = 20*time.Millisecond, time.Millisecond
	fs.set(func(f *flakyStore) { f.conflicts = 1 << 30 })

	_, err := l.TokenFor(context.Background(), "U1")
	require.ErrorIs(t, err, errRefreshContended)
	require.NotErrorIs(t, err, ErrNotLinked)
	require.Equal(t, 0, stub.issued(), "nothing was presented at muster")
	require.Equal(t, "refresh-0", storedRefresh(t, mem))
}

// A sign-in that lands while this process refreshes the old link wins: the
// rotated old link is not written over it, and the new link's id_token is
// served.
func TestTokenForKeepsSignInThatLandsDuringRefresh(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	require.NoError(t, store.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "refresh-0"}))
	relinked := &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "relinked", IDToken: makeIDToken("muster-sub", time.Now().Add(time.Hour)), Expiry: time.Now().Add(time.Hour)}
	stub.mu.Lock()
	stub.onRefresh = func() { _ = store.Put("U1", relinked) }
	stub.mu.Unlock()
	l := newTestLinker(t, stub, store, nil)

	idToken, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, relinked.IDToken, idToken)
	require.Equal(t, "relinked", storedRefresh(t, store))
}

// invalid_grant drops the link only while the store holds the refused token:
// a sign-in that landed meanwhile is kept and its id_token served instead of
// a sign-in prompt.
func TestTokenForInvalidGrantKeepsSignInThatLandedMeanwhile(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	stub.failRefresh = true
	store := NewMemStore()
	require.NoError(t, store.Put("U1", &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "dead"}))
	relinked := &Link{Sub: "muster-sub", Email: "a@example.com", RefreshToken: "relinked", IDToken: makeIDToken("muster-sub", time.Now().Add(time.Hour)), Expiry: time.Now().Add(time.Hour)}
	stub.mu.Lock()
	stub.onRefresh = func() { _ = store.Put("U1", relinked) }
	stub.mu.Unlock()
	l := newTestLinker(t, stub, store, nil)

	idToken, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, relinked.IDToken, idToken)
	require.Equal(t, "relinked", storedRefresh(t, store))
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
