package muster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func staticToken(token string) TokenSource {
	return func(context.Context, string) (string, error) { return token, nil }
}

func TestConnectorsStatus_Cached(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.statusJSON = statusFixture
	s := &Connectors{Client: client, Token: staticToken("user-token"), TTL: time.Hour}

	first, err := s.Status(t.Context(), "U1")
	require.NoError(t, err)
	require.Len(t, first, 3)

	_, err = s.Status(t.Context(), "U1")
	require.NoError(t, err)

	fake.mu.Lock()
	reads := fake.readCalls
	fake.mu.Unlock()
	require.Equal(t, 1, reads, "second Status within the TTL must hit the cache")
}

func TestConnectorsStatus_CacheExpires(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.statusJSON = statusFixture
	s := &Connectors{Client: client, Token: staticToken("user-token"), TTL: time.Millisecond}

	_, err := s.Status(t.Context(), "U1")
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	_, err = s.Status(t.Context(), "U1")
	require.NoError(t, err)

	fake.mu.Lock()
	reads := fake.readCalls
	fake.mu.Unlock()
	require.Equal(t, 2, reads)
}

func TestConnectorsFreshStatus_BypassesCache(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.statusJSON = statusFixture
	s := &Connectors{Client: client, Token: staticToken("user-token"), TTL: time.Hour}

	_, err := s.Status(t.Context(), "U1")
	require.NoError(t, err)
	_, err = s.FreshStatus(t.Context(), "U1")
	require.NoError(t, err)

	fake.mu.Lock()
	reads := fake.readCalls
	fake.mu.Unlock()
	require.Equal(t, 2, reads)
}

// Token-source errors (e.g. musterlink.ErrNotLinked) must propagate unwrapped
// so the adapter can match them.
func TestConnectors_TokenErrorPassthrough(t *testing.T) {
	_, client := newFakeMuster(t)
	sentinel := errors.New("not linked")
	s := &Connectors{Client: client, Token: func(context.Context, string) (string, error) {
		return "", sentinel
	}}

	_, err := s.Status(t.Context(), "U1")
	require.ErrorIs(t, err, sentinel)
	_, err = s.LoginURL(t.Context(), "U1", "pro")
	require.ErrorIs(t, err, sentinel)
	require.ErrorIs(t, s.Logout(t.Context(), "U1", "pro"), sentinel)
}

func TestConnectorsLogout_InvalidatesCache(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.statusJSON = statusFixture
	fake.logoutText = "Disconnected."
	s := &Connectors{Client: client, Token: staticToken("user-token"), TTL: time.Hour}

	_, err := s.Status(t.Context(), "U1")
	require.NoError(t, err)
	require.NoError(t, s.Logout(t.Context(), "U1", "gazelle-mcp-pro"))
	_, err = s.Status(t.Context(), "U1")
	require.NoError(t, err)

	fake.mu.Lock()
	reads := fake.readCalls
	fake.mu.Unlock()
	require.Equal(t, 2, reads, "logout must drop the cached status")
}

// Per-user caching: one user's read never serves another user.
func TestConnectorsStatus_PerUser(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.statusJSON = statusFixture
	s := &Connectors{Client: client, Token: staticToken("user-token"), TTL: time.Hour}

	_, err := s.Status(t.Context(), "U1")
	require.NoError(t, err)
	_, err = s.Status(t.Context(), "U2")
	require.NoError(t, err)

	fake.mu.Lock()
	reads := fake.readCalls
	fake.mu.Unlock()
	require.Equal(t, 2, reads)
}
