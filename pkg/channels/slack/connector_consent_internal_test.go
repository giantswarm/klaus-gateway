package slack

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

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

func TestSignInLinkText(t *testing.T) {
	t.Run("ascii domain", func(t *testing.T) {
		text := signInLinkText("https://muster.example/oauth/proxy/start?state=a&b=1")
		require.Equal(t, "Opens *muster.example*\n`https://muster.example/oauth/proxy/start?state=a&amp;b=1`", text,
			"the whole link as written, in code; its domain in bold")
	})
	t.Run("punycode domain", func(t *testing.T) {
		text := signInLinkText("https://xn--mster-kva.example/start")
		require.Contains(t, text, "Opens *xn--mster-kva.example*")
		require.Contains(t, text, "`https://xn--mster-kva.example/start`")
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

// A URL elicitation's link button is neutral and names the host it opens, an
// internationalized one in its punycode form.
func TestElicitationOpenButtonLabel_NamesTheHost(t *testing.T) {
	require.Equal(t, "Open ↗ muster.example", elicitationOpenButtonLabel("https://muster.example/approvals/1"))
	require.Equal(t, "Open ↗ xn--mster-kva.example", elicitationOpenButtonLabel("https://müster.example/approvals/1"))
	require.Equal(t, "Open ↗", elicitationOpenButtonLabel("not a link"))
}

// The server's message is the heading: one line, escaped, capped; a server
// that sent none is named instead.
func TestElicitationHeading(t *testing.T) {
	require.Equal(t, "*Alert Review wants to acknowledge &lt;P123&gt;.*", elicitationHeading("muster", "Alert Review wants\nto acknowledge <P123>."))
	require.Equal(t, "*muster* asks you to open a link so the agent can continue.", elicitationHeading("muster", "  "))
	require.LessOrEqual(t, utf8.RuneCountInString(elicitationHeading("muster", strings.Repeat("a", 2*signInMessageMaxRunes))), signInMessageMaxRunes+2)
}

// A declinable prompt is a URL elicitation's: Open, Done and Not now, Done and
// Not now carrying the completion state.
func TestConnectPromptBlocks_DeclinableHasDone(t *testing.T) {
	blocks := connectPromptBlocks("text", elicitationOpenButtonLabel("https://muster.example/s"), "https://muster.example/s", "state", true, false)
	elements := blocks[1].(map[string]any)[bkElements].([]any)
	var ids, values []string
	for _, el := range elements {
		button := el.(map[string]any)
		ids = append(ids, button[bkActionID].(string))
		values = append(values, button[bkValue].(string))
	}
	require.Equal(t, []string{connectorConnect, elicitationDone, connectorDecline}, ids)
	require.Equal(t, []string{"state", "state", "state"}, values)
}

// muster's return resumes only the prompt whose link the gateway issued it
// for (security review M1-KG-S-01): the elicitationId alone, which the
// server that sent it chose or knows, takes nothing, and two people's
// prompts with the same elicitationId are kept apart by their own keys.
func TestElicitationReturn_OnlyThePromptItWasIssuedFor(t *testing.T) {
	a := &Adapter{MusterURL: "https://muster.example", ElicitationReturnURL: "https://gw.example" + ElicitationReturnPath}
	_, link1 := a.mintPrompt(connectorCompletion{slackUser: "UVICTIM1", channel: "C1", threadTS: "1.1", signInTask: "t1", elicitationID: "attacker-chosen"}, "https://muster.example/approvals/a")
	_, link2 := a.mintPrompt(connectorCompletion{slackUser: "UVICTIM2", channel: "C2", threadTS: "2.2", signInTask: "t2", elicitationID: "attacker-chosen"}, "https://muster.example/approvals/a")
	_, link3 := a.mintPrompt(connectorCompletion{slackUser: "UVICTIM3", channel: "C3", threadTS: "3.3", signInTask: "t3", elicitationID: "attacker-chosen"}, "https://mcp.third-party.example/consent")
	require.Equal(t, "https://mcp.third-party.example/consent", link3, "another server's link gets no return")

	_, ok := a.takeElicitationReturn("", "attacker-chosen")
	require.False(t, ok, "the id alone takes nothing")

	key := func(link string) string {
		u, err := url.Parse(link)
		require.NoError(t, err)
		back, err := url.Parse(u.Query().Get(muster.ReturnLinkParam))
		require.NoError(t, err)
		return back.Query().Get(returnPromptParam)
	}
	key1, key2 := key(link1), key(link2)
	require.NotEmpty(t, key1)
	require.NotEqual(t, key1, key2)

	entry, ok := a.takeElicitationReturn(key1, "attacker-chosen")
	require.True(t, ok)
	require.Equal(t, "UVICTIM1", entry.slackUser, "only the first person's prompt")
	_, ok = a.takeElicitationReturn(key1, "attacker-chosen")
	require.False(t, ok, "a replay takes nothing")
	_, ok = a.takeElicitationReturn(key2, "another-id")
	require.False(t, ok, "a key with another elicitation's id takes nothing")
	entry, ok = a.takeElicitationReturn(key2, "")
	require.True(t, ok, "a sign-in's return carries no elicitationId")
	require.Equal(t, "UVICTIM2", entry.slackUser)
}

// A muster link that names a redirect of its own loses it (security review
// M1-KG-S-04): the prompt opens muster's link with the gateway's return, or
// with no redirect at all when no return is configured.
func TestPromptLink_DropsTheLinksOwnRedirect(t *testing.T) {
	hostile := "https://muster.example/approvals/a?redirect=" + url.QueryEscape("https://portal.example/any/page")
	a := &Adapter{MusterURL: "https://muster.example", ElicitationReturnURL: "https://gw.example" + ElicitationReturnPath}
	shown, key := a.promptLink(hostile)
	require.NotEmpty(t, key)
	require.NotContains(t, shown, "portal.example")
	require.Contains(t, shown, url.QueryEscape("https://gw.example"+ElicitationReturnPath+"?"+returnPromptParam+"="+key))

	shown, key = (&Adapter{MusterURL: "https://muster.example"}).promptLink(hostile)
	require.Empty(t, key)
	require.Equal(t, "https://muster.example/approvals/a", shown)
}

// A host too long for the Open button is cut on the left, at a label, so the
// button always shows the registrable domain the link opens (security review
// M1-KG-S-02); a host whose last two labels alone do not fit is left off.
func TestElicitationOpenButtonLabel_KeepsTheRightEndOfALongHost(t *testing.T) {
	link := "https://muster.example.com." + strings.Repeat("a", 60) + ".evil.example/approvals/x"
	require.NoError(t, muster.ValidateSignInURL(link))
	label := elicitationOpenButtonLabel(link)
	require.LessOrEqual(t, utf8.RuneCountInString(label), 75)
	require.Equal(t, "Open ↗ …evil.example", label)

	label = elicitationOpenButtonLabel("https://muster.example.com.evil-" + strings.Repeat("b", 63) + ".example/x")
	require.Equal(t, "Open ↗", label, "the registrable domain does not fit: the host is left to the text")
	require.Equal(t, "Open ↗ a.b.example", elicitationOpenButtonLabel("https://a.b.example/x"))
}

// Text a relayed elicitation chose (its message, its server's name) shows as
// written and nothing more (security review M1-KG-S-03): bidi overrides and
// zero-width characters are dropped, and Slack's formatting characters cannot
// end the heading's bold or start a format of their own.
func TestElicitationHeading_NeutralisesFormattingAndDirection(t *testing.T) {
	h := elicitationHeading("third-party", "Approve* _safe_ `x` ‮evil‬ ​hidden ~strike~ *Asked by muster")
	require.NotContains(t, h, "‮")
	require.NotContains(t, h, "‬")
	require.NotContains(t, h, "​")
	require.NotContains(t, h, "`")
	require.NotContains(t, h, "~")
	inner := strings.TrimSuffix(strings.TrimPrefix(h, "*"), "*")
	require.NotContains(t, inner, "*", "only the heading's own bold")
	require.NotContains(t, inner, " _", "no italic can open")
	require.Contains(t, inner, "Approve")
	require.Contains(t, inner, "evil")

	h = elicitationHeading("x_github *muster*‮", "")
	require.Contains(t, h, "x_github", "an underscore inside a word formats nothing and stays")
	require.NotContains(t, strings.TrimPrefix(h, "*"), "*muster")
	require.NotContains(t, h, "‮")
}
