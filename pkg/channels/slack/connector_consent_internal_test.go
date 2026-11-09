package slack

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The first click of the person a prompt is for is their consent; another
// person's click, a second click, an unknown or an expired state continue
// nothing.
func TestConnectorConsent_FirstClickOfThePersonOnly(t *testing.T) {
	a := &Adapter{}
	stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: "U1", channel: "C1", threadTS: "1.0", review: "rv-1"})

	_, ok := a.takeConnectorConsent("U2", stateID)
	require.False(t, ok, "another person's click is not the consent")

	entry, ok := a.takeConnectorConsent("U1", stateID)
	require.True(t, ok)
	require.Equal(t, "rv-1", entry.review)

	_, ok = a.takeConnectorConsent("U1", stateID)
	require.False(t, ok, "a second click only opens the link again")

	_, ok = a.takeConnectorConsent("U1", "unknown")
	require.False(t, ok)

	expired := a.mintConnectorCompletion(connectorCompletion{slackUser: "U1"})
	a.connectorCompletionsMu.Lock()
	stored := a.connectorCompletions[expired]
	stored.expires = time.Now().Add(-time.Second)
	a.connectorCompletions[expired] = stored
	a.connectorCompletionsMu.Unlock()
	_, ok = a.takeConnectorConsent("U1", expired)
	require.False(t, ok, "an expired state continues nothing")
}

func TestConnectorConsent_DroppedStateContinuesNothing(t *testing.T) {
	a := &Adapter{}
	stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: "U1"})
	a.dropConnectorCompletions([]string{stateID})
	_, ok := a.takeConnectorConsent("U1", stateID)
	require.False(t, ok)
}

func TestSignInLinkText(t *testing.T) {
	t.Run("ascii domain", func(t *testing.T) {
		text := signInLinkText("https://muster.example/oauth/proxy/start?state=a&b=1")
		require.Equal(t, "Opens *muster.example*\n`https://muster.example/oauth/proxy/start?state=a&amp;b=1`", text,
			"the whole link as written, in code; its domain in bold")
	})
	t.Run("punycode domain", func(t *testing.T) {
		text := signInLinkText("https://xn--mster-jua.example/start")
		require.Contains(t, text, "Opens *xn--mster-jua.example*")
		require.Contains(t, text, "`https://xn--mster-jua.example/start`")
		require.Contains(t, text, signInLinkPunycodeWarning)
	})
	t.Run("unicode domain shows its punycode form", func(t *testing.T) {
		text := signInLinkText("https://müster.example/start")
		require.Contains(t, text, "Opens *xn--mster-kva.example*")
		require.Contains(t, text, "`https://müster.example/start`", "the link itself stays as given")
		require.Contains(t, text, signInLinkPunycodeWarning)
	})
}

func TestSignInPromptText(t *testing.T) {
	text := signInPromptText("*muster* asks you to sign in:", "Sign in to <github>.\nNow.", "https://muster.example/s", "Then continue.")
	require.Equal(t, "*muster* asks you to sign in:\n>Sign in to &lt;github&gt;.\n>Now.\nOpens *muster.example*\n`https://muster.example/s`\nThen continue.", text)
}
