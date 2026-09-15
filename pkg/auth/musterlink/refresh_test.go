package musterlink

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func stubCalls(stub *musterStub) int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.counter
}

// A token a turn was handed and that is within refreshAhead of expiry is
// refreshed by the sweep, off any turn: the cached id_token and its expiry
// move, the stored refresh token rotates, and the next TokenFor is a cache
// hit with no call to muster. A second sweep finds nothing due.
func TestRefreshDue_RefreshesServedTokenAheadOfExpiry(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	require.NoError(t, store.Put("U1", &Link{Sub: "muster-sub", RefreshToken: "refresh-0", IDToken: "seeded", Expiry: time.Now().Add(time.Hour)}))
	l := newTestLinker(t, stub, store, nil)
	t.Cleanup(func() { _ = l.Close() })

	// The turn serves the still-valid token: a cache hit, no refresh.
	tok, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, 0, stubCalls(stub))
	require.Equal(t, 0, l.RefreshDue(context.Background()), "an hour from expiry nothing is due")

	// Fifty-six minutes later the token is four minutes from expiry: due.
	l.now = func() time.Time { return time.Now().Add(56 * time.Minute) }
	require.Equal(t, 1, l.RefreshDue(context.Background()))
	require.Equal(t, 1, stubCalls(stub), "the sweep spent one refresh")
	stored, err := store.Get("U1")
	require.NoError(t, err)
	require.Equal(t, "refresh-1", stored.RefreshToken, "the rotated refresh token is persisted")
	require.NotEqual(t, tok, stored.IDToken)
	require.True(t, stored.Expiry.After(time.Now().Add(55*time.Minute)), "the new id_token's expiry is Dex's, an hour from its mint")

	// Nothing left to do (the clock back at the stub's, which minted the new
	// token an hour from now), and the turn finds the fresh token without a
	// call.
	l.now = time.Now
	require.Equal(t, 0, l.RefreshDue(context.Background()))
	fresh, err := l.TokenFor(context.Background(), "U1")
	require.NoError(t, err)
	require.Equal(t, stored.IDToken, fresh)
	require.Equal(t, 1, stubCalls(stub), "TokenFor after the sweep is a cache hit")
}

// The sweep leaves alone what no turn asked for: a link only read from the
// store, and one served longer ago than the window — those refresh on their
// next turn, as before. A link with an unknown expiry is TokenFor's to sort
// out.
func TestRefreshDue_OnlyServedLinksWithinTheWindow(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	soon := time.Now().Add(2 * time.Minute)
	require.NoError(t, store.Put("idle", &Link{RefreshToken: "refresh-idle", IDToken: "idle-token", Expiry: soon}))
	require.NoError(t, store.Put("stale", &Link{RefreshToken: "refresh-stale", IDToken: "stale-token", Expiry: soon}))
	require.NoError(t, store.Put("unknown", &Link{RefreshToken: "refresh-unknown"}))
	l := newTestLinker(t, stub, store, nil)
	t.Cleanup(func() { _ = l.Close() })

	// "idle" is loaded into the process copy but never handed to a turn.
	_, err := l.load("idle")
	require.NoError(t, err)
	// "stale" was served three days ago.
	l.now = func() time.Time { return time.Now().Add(-3 * 24 * time.Hour) }
	_, err = l.TokenFor(context.Background(), "stale")
	require.NoError(t, err)
	// "unknown" was served now but has no cached expiry to refresh ahead of.
	l.now = time.Now
	_, err = l.TokenFor(context.Background(), "unknown") // refreshes on the turn: expiry unknown
	require.NoError(t, err)
	before := stubCalls(stub)

	require.Equal(t, 0, l.RefreshDue(context.Background()))
	require.Equal(t, before, stubCalls(stub), "the sweep spent nothing on an idle, a stale or a fresh link")
}

// A refusal of the refresh token during the sweep drops the link the way a
// turn's refresh would; the person is asked to sign in on their next
// message. A transient failure keeps the link and the cached token for the
// next sweep, and past expiry TokenFor refreshes on the turn as before.
func TestRefreshDue_RefusalDropsTransientKeeps(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	store := NewMemStore()
	soon := time.Now().Add(2 * time.Minute)
	require.NoError(t, store.Put("dead", &Link{RefreshToken: "dead", IDToken: makeIDToken("muster-sub", soon), Expiry: soon}))
	require.NoError(t, store.Put("flaky", &Link{RefreshToken: "refresh-0", IDToken: makeIDToken("muster-sub", soon), Expiry: soon}))
	l := newTestLinker(t, stub, store, nil)
	t.Cleanup(func() { _ = l.Close() })
	for _, id := range []string{"dead", "flaky"} {
		_, err := l.TokenFor(context.Background(), id)
		require.NoError(t, err, "two minutes from expiry the cached token is still served")
	}

	stub.mu.Lock()
	stub.failRefresh5xx = true
	stub.mu.Unlock()
	require.Equal(t, 0, l.RefreshDue(context.Background()))
	flaky, err := store.Get("flaky")
	require.NoError(t, err, "a transient failure keeps the link")
	require.Equal(t, "refresh-0", flaky.RefreshToken)
	require.Len(t, l.dueForRefresh(l.now()), 2, "both stay due for the next sweep")

	stub.mu.Lock()
	stub.failRefresh5xx = false
	stub.failRefresh = true
	stub.mu.Unlock()
	require.Equal(t, 0, l.RefreshDue(context.Background()))
	_, err = store.Get("dead")
	require.ErrorIs(t, err, ErrNotLinked, "a refusal drops the link")
	_, err = l.TokenFor(context.Background(), "dead")
	require.ErrorIs(t, err, ErrNotLinked, "the next turn prompts a sign-in")
}

// Close stops the refresher and is idempotent; New starts it.
func TestRefresher_StopsOnClose(t *testing.T) {
	stub := newMusterStub(t, "klaus-gateway", "a@example.com", "muster-sub")
	l := newTestLinker(t, stub, NewMemStore(), nil)
	require.NoError(t, l.Close())
	require.NoError(t, l.Close())
	select {
	case <-l.refreshDone:
	default:
		t.Fatal("the refresher is still running after Close")
	}
}
