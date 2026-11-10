package slack

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// The first click of the person a prompt is for is their consent; another
// person's click, a second click, an unknown or an expired state continue
// nothing.
func TestConnectorConsent_FirstClickOfThePersonOnly(t *testing.T) {
	a := &Adapter{}
	stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: "U1", channel: "C1", threadTS: "1.0", review: "rv-1"})

	_, ok := a.takeConnectorAnswer("U2", stateID)
	require.False(t, ok, "another person's click is not the consent")

	entry, ok := a.takeConnectorAnswer("U1", stateID)
	require.True(t, ok)
	require.Equal(t, "rv-1", entry.review)

	_, ok = a.takeConnectorAnswer("U1", stateID)
	require.False(t, ok, "a second click only opens the link again")

	_, ok = a.takeConnectorAnswer("U1", "unknown")
	require.False(t, ok)

	expired := a.mintConnectorCompletion(connectorCompletion{slackUser: "U1"})
	a.connectorCompletionsMu.Lock()
	stored := a.connectorCompletions[expired]
	stored.expires = time.Now().Add(-time.Second)
	a.connectorCompletions[expired] = stored
	a.connectorCompletionsMu.Unlock()
	_, ok = a.takeConnectorAnswer("U1", expired)
	require.False(t, ok, "an expired state continues nothing")
}

func TestConnectorConsent_DroppedStateContinuesNothing(t *testing.T) {
	a := &Adapter{}
	stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: "U1"})
	a.dropConnectorCompletions([]string{stateID})
	_, ok := a.takeConnectorAnswer("U1", stateID)
	require.False(t, ok)
}

// A thread's held message is its latest prompt's, while that state is alive
// and unanswered; a click that already continues any of the thread's states
// runs its own message, so none is handed out. Every state is forgotten.
func TestConnectorConsent_TakeHeldStart(t *testing.T) {
	mint := func(a *Adapter, text string) string {
		return a.mintConnectorCompletion(connectorCompletion{slackUser: "U1", held: &heldMessage{SlackChannel: "C1", Msg: channels.InboundMessage{Text: text}}})
	}
	expire := func(a *Adapter, stateID string) {
		a.connectorCompletionsMu.Lock()
		defer a.connectorCompletionsMu.Unlock()
		stored := a.connectorCompletions[stateID]
		stored.expires = time.Now().Add(-time.Second)
		a.connectorCompletions[stateID] = stored
	}

	t.Run("the latest prompt's message", func(t *testing.T) {
		a := &Adapter{}
		states := []string{mint(a, "first"), mint(a, "second")}
		held, continuing := a.takeHeldStart(states)
		require.False(t, continuing)
		require.NotNil(t, held)
		require.Equal(t, "second", held.Msg.Text)
		_, ok := a.takeConnectorAnswer("U1", states[0])
		require.False(t, ok, "every state is forgotten")
	})
	t.Run("expired", func(t *testing.T) {
		a := &Adapter{}
		states := []string{mint(a, "first"), mint(a, "second")}
		expire(a, states[1])
		held, continuing := a.takeHeldStart(states)
		require.False(t, continuing)
		require.Nil(t, held, "an earlier prompt's message is not run in its place")
	})
	t.Run("a click continues it", func(t *testing.T) {
		a := &Adapter{}
		states := []string{mint(a, "first"), mint(a, "second")}
		_, ok := a.takeConnectorAnswer("U1", states[0])
		require.True(t, ok)
		held, continuing := a.takeHeldStart(states)
		require.True(t, continuing)
		require.Nil(t, held, "the click's continuation runs its own message")
	})
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

func TestSignInLinkText_Warnings(t *testing.T) {
	t.Run("upper-case ascii domain gets no warning", func(t *testing.T) {
		text := signInLinkText("https://Muster.Example/start")
		require.NotContains(t, text, signInLinkPunycodeWarning)
	})
	t.Run("unicode domain without a punycode form is shown as written, with the warning", func(t *testing.T) {
		link := "https://\u0300muster.example/start"
		require.NoError(t, muster.ValidateSignInURL(link))
		text := signInLinkText(link)
		require.Contains(t, text, "Opens *\u0300muster.example*")
		require.Contains(t, text, signInLinkPunycodeWarning)
	})
}

func TestSignInPromptText_LinkIsNeverCut(t *testing.T) {
	link := "https://muster.example/s?" + strings.Repeat("a", muster.SignInURLMaxBytes-len("https://muster.example/s?"))
	require.NoError(t, muster.ValidateSignInURL(link))
	lead := fmt.Sprintf(signInPromptLead, strings.Repeat("&", signInServerNameMaxRunes))
	text := signInPromptText(lead, strings.Repeat("<x>\n", 400), link, signInPromptTail)
	require.LessOrEqual(t, utf8.RuneCountInString(text), slackSectionTextMax)
	require.Contains(t, text, "`"+link+"`")
	require.True(t, strings.HasSuffix(text, signInPromptTail))
}

func TestConnectPromptBlocks_TextIsVerbatim(t *testing.T) {
	blocks := connectPromptBlocks("see https://evil.example", signInButtonLabel, "https://muster.example/s", "state", false, false)
	section := blocks[0].(map[string]any)[bkText].(map[string]any)
	require.Equal(t, true, section[bkVerbatim], "Slack links no URL of the text, only the button's")
}

func TestAuthorizeRow_ShowsTheLinkItOpens(t *testing.T) {
	row := authorizeRow(grantServer{Name: "github", Status: grantStatusAuthNeeded, Kind: grantKindGrant, AuthURL: "https://muster.example/grant?s=1"})
	text := row[bkText].(map[string]any)
	require.Equal(t, true, text[bkVerbatim])
	require.Contains(t, text[bkText], "Opens *muster.example*\n`https://muster.example/grant?s=1`")
	require.Equal(t, "https://muster.example/grant?s=1", row[bkAccessory].(map[string]any)[bkURL])

	row = authorizeRow(grantServer{Name: "github", Status: grantStatusAuthNeeded, Kind: grantKindGrant, AuthURL: "http://muster.example/grant"})
	require.NotContains(t, row, bkAccessory, "a link that is not one the app opens gets no button")
	require.Contains(t, row[bkText].(map[string]any)[bkText], authorizeLinkUnusable)
}
