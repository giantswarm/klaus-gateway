package slack

import (
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConnectorCompletion_MintLookupAndTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &Adapter{}
		stateID := a.mintConnectorCompletion("U1", "pro", "D1", "100.000")
		require.NotEmpty(t, stateID)

		entry, rewriteNow, ok := a.recordConnectorConnectClick("U1", stateID, "https://hooks.example/r1")
		require.True(t, ok)
		require.False(t, rewriteNow, "no landing yet, the click has nothing to rewrite")
		require.Equal(t, "pro", entry.server)
		require.Equal(t, "D1", entry.channel)
		require.Equal(t, "100.000", entry.threadTS)

		time.Sleep(connectorCompletionTTL + time.Minute)

		_, _, ok = a.recordConnectorConnectClick("U1", stateID, "https://hooks.example/r1")
		require.False(t, ok, "an expired state must not accept a click")
		_, _, _, ok = a.completeConnectorLanding(stateID)
		require.False(t, ok, "an expired state must not complete")

		a.mintConnectorCompletion("U2", "pro", "D2", "200.000")
		a.connectorCompletionsMu.Lock()
		defer a.connectorCompletionsMu.Unlock()
		require.Len(t, a.connectorCompletions, 1, "a fresh mint sweeps expired siblings")
	})
}

func TestConnectorCompletion_ClickThenLanding(t *testing.T) {
	a := &Adapter{}
	stateID := a.mintConnectorCompletion("U1", "pro", "D1", "100.000")

	_, rewriteNow, ok := a.recordConnectorConnectClick("U1", stateID, "https://hooks.example/r1")
	require.True(t, ok)
	require.False(t, rewriteNow)

	entry, resume, rewrite, ok := a.completeConnectorLanding(stateID)
	require.True(t, ok)
	require.True(t, resume, "first landing fires the resume")
	require.True(t, rewrite, "the landing arrived second and owns the rewrite")
	require.Equal(t, "https://hooks.example/r1", entry.responseURL)
}

func TestConnectorCompletion_LandingThenClick(t *testing.T) {
	a := &Adapter{}
	stateID := a.mintConnectorCompletion("U1", "pro", "D1", "100.000")

	_, resume, rewrite, ok := a.completeConnectorLanding(stateID)
	require.True(t, ok)
	require.True(t, resume)
	require.False(t, rewrite, "no response_url recorded yet, nothing to rewrite")

	entry, rewriteNow, ok := a.recordConnectorConnectClick("U1", stateID, "https://hooks.example/r1")
	require.True(t, ok)
	require.True(t, rewriteNow, "the click arrived second and owns the rewrite")
	require.Equal(t, "pro", entry.server)
}

func TestConnectorCompletion_LandingReloadIsIdempotent(t *testing.T) {
	a := &Adapter{}
	stateID := a.mintConnectorCompletion("U1", "pro", "D1", "100.000")
	a.recordConnectorConnectClick("U1", stateID, "https://hooks.example/r1")

	_, resume, rewrite, ok := a.completeConnectorLanding(stateID)
	require.True(t, ok)
	require.True(t, resume)
	require.True(t, rewrite)

	_, resume, rewrite, ok = a.completeConnectorLanding(stateID)
	require.True(t, ok, "a reload still renders the signed-in page")
	require.False(t, resume, "a reload must not re-dispatch the resume")
	require.False(t, rewrite, "the prompt is already rewritten")
}

func TestConnectorCompletion_RejectsUnknownStateAndWrongUser(t *testing.T) {
	a := &Adapter{}
	stateID := a.mintConnectorCompletion("U1", "pro", "D1", "100.000")

	_, _, ok := a.recordConnectorConnectClick("U1", "bogus", "https://hooks.example/r1")
	require.False(t, ok, "an unknown state (or a legacy server-name value) is a no-op")
	_, _, ok = a.recordConnectorConnectClick("U2", stateID, "https://hooks.example/r1")
	require.False(t, ok, "a clicker other than the prompted user is rejected")
	_, _, _, ok = a.completeConnectorLanding("bogus")
	require.False(t, ok)
}

func TestDecorateConnectorLoginURL(t *testing.T) {
	decorated, err := decorateConnectorLoginURL(
		"https://muster.example/authorize?state=abc", "https://gw.example", "sid-1")
	require.NoError(t, err)

	parsed, err := url.Parse(decorated)
	require.NoError(t, err)
	require.Equal(t, "abc", parsed.Query().Get("state"), "existing query params survive")
	require.Equal(t, "https://gw.example/connectors/complete?s=sid-1", parsed.Query().Get("redirect"))

	decorated, err = decorateConnectorLoginURL(
		"https://muster.example/authorize", "https://gw.example/", "s id")
	require.NoError(t, err)
	parsed, err = url.Parse(decorated)
	require.NoError(t, err)
	require.Equal(t, "https://gw.example/connectors/complete?s=s+id", parsed.Query().Get("redirect"),
		"trailing-slash base does not double the separator; the state ID is escaped")

	_, err = decorateConnectorLoginURL("://bad", "https://gw.example", "sid")
	require.Error(t, err)
	_, err = decorateConnectorLoginURL("/relative/path", "https://gw.example", "sid")
	require.Error(t, err, "a non-absolute login URL is rejected")
}
