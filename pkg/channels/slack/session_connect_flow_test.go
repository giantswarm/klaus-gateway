package slack_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

const sessionConnectLink = "https://muster.example/oauth/proxy/start?state=abc"

func sessionConnectChallenge() *pkga2a.ConnectRequiredError {
	return &pkga2a.ConnectRequiredError{Server: "agent-manager", LoginURL: sessionConnectLink}
}

// sessionConnectAdapter is decoratedConnectorAdapter in channel mode with the
// adapter's log captured and a users.info that answers the person's e-mail,
// so the turn rewrites the sender the way it does on an installation.
func sessionConnectAdapter(t *testing.T, gw *stubGateway, opts ...func(*slackadapter.Adapter)) (*fakeSlackAPI, *httptest.Server, *syncBuffer) {
	t.Helper()
	fake := newFakeSlackAPI()
	fake.setResponse("users.info", `{"ok":true,"user":{"id":"U1","profile":{"email":"u1@example.com"}}}`)
	logs := &syncBuffer{}
	opts = append([]func(*slackadapter.Adapter){channelMode, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
		a.ConnectorPrompts = true
		a.PublicBaseURL = "https://gw.example"
		a.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}}, opts...)
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, opts...)
	return fake, srv, logs
}

// logLine is the first captured log line that carries every needle.
func logLine(logs *syncBuffer, needles ...string) string {
	for _, line := range strings.Split(logs.String(), "\n") {
		found := true
		for _, needle := range needles {
			if !strings.Contains(line, needle) {
				found = false
				break
			}
		}
		if found {
			return line
		}
	}
	return ""
}

// connectButtonIn walks calls for the Connect button and returns its url and
// value, with the call it was found in.
func connectButtonIn(calls []recordedCall) (buttonURL, buttonValue string, call recordedCall, ok bool) {
	for _, c := range calls {
		blocks, _ := c.params["blocks"].([]any)
		for _, block := range blocks {
			blockMap, _ := block.(map[string]any)
			elements, _ := blockMap["elements"].([]any)
			for _, element := range elements {
				button, _ := element.(map[string]any)
				if button["action_id"] == "connector_connect" {
					buttonURL, _ = button["url"].(string)
					buttonValue, _ = button["value"].(string)
					return buttonURL, buttonValue, c, true
				}
			}
		}
	}
	return "", "", recordedCall{}, false
}

// stateOf is the completion state a decorated login URL lands with.
func stateOf(t *testing.T, buttonURL string) string {
	t.Helper()
	parsed, err := url.Parse(buttonURL)
	require.NoError(t, err)
	landing, err := url.Parse(parsed.Query().Get("redirect"))
	require.NoError(t, err)
	return landing.Query().Get("s")
}

// postsTo are the chat.postMessage calls into channel.
func postsTo(fake *fakeSlackAPI, channel string) []recordedCall {
	var calls []recordedCall
	for _, c := range fake.pathCalls("chat.postMessage") {
		if c.params["channel"] == channel {
			calls = append(calls, c)
		}
	}
	return calls
}

// updatesIn are the chat.update calls into channel.
func updatesIn(fake *fakeSlackAPI, channel string) []recordedCall {
	var calls []recordedCall
	for _, c := range fake.pathCalls("chat.update") {
		if c.params["channel"] == channel {
			calls = append(calls, c)
		}
	}
	return calls
}

// A Session start muster refuses with its sign-in challenge for a server, in
// a channel thread: the Connect prompt goes to the person as an ephemeral,
// the same card goes to their DM led by where it was asked, and a line
// everyone sees says whom the thread waits for and where the button is. No
// failure note. The message is held as admitted: the sign-in's landing runs
// it as the thread's first turn, with nothing retyped, also when the turn had
// resolved the sender to their e-mail, and rewrites the line and the card.
func TestSessionConnect_ChannelThreadPromptsEverywhereAndRunsTheHeldMessage(t *testing.T) {
	var dispatched struct {
		mu   sync.Mutex
		msgs []channels.InboundMessage
	}
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	gw.onDispatch = func(msg channels.InboundMessage) {
		dispatched.mu.Lock()
		dispatched.msgs = append(dispatched.msgs, msg)
		dispatched.mu.Unlock()
	}
	fake, srv, logs := sessionConnectAdapter(t, gw)
	responseSrv, capturedBody := newResponseURLCapture(t)

	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "140.000", ""))

	var buttonURL, stateID string
	var ok bool
	require.Eventually(t, func() bool {
		buttonURL, stateID, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond, "the ephemeral Connect prompt is posted")
	ephemeral := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, ephemeral, "*agent-manager* needs you to connect your account once")
	require.Contains(t, ephemeral, "your message is sent to the agent")
	require.NotEqual(t, "agent-manager", stateID, "the ephemeral's button value is the completion state")
	require.Equal(t, stateID, stateOf(t, buttonURL), "the state the link lands with")
	parsed, err := url.Parse(buttonURL)
	require.NoError(t, err)
	require.Equal(t, "abc", parsed.Query().Get("state"), "muster's own link survives decoration")

	dmURL, dmValue, dmCard, ok := connectButtonIn(postsTo(fake, "U1"))
	require.True(t, ok, "the same card goes to the person's DM")
	require.Equal(t, buttonURL, dmURL, "the DM card carries the same decorated link")
	require.Equal(t, "agent-manager", dmValue, "the card's button carries the server, not the state: its click records no response_url")
	require.Contains(t, fmt.Sprint(dmCard.params["blocks"]), "Asked in a thread in <#C1>.", "the card says where it was asked")
	require.Len(t, postsTo(fake, "U1"), 1)

	line := allText(postsTo(fake, "C1"))
	require.Contains(t, line, "Waiting for <@U1> to connect *agent-manager* (the Connect button is in a direct message from the app)")
	require.NotContains(t, line, "muster.example", "the line carries no link")
	require.NotContains(t, line, "The turn failed", "a consent to give is not a failure")
	for _, c := range postsTo(fake, "C1") {
		require.Equal(t, "140.000", c.params["thread_ts"], "the line is in the thread")
	}
	require.Equal(t, 1, gw.dispatchCount())
	dispatched.mu.Lock()
	require.Equal(t, "u1@example.com", dispatched.msgs[0].Subject, "the turn ran under the resolved e-mail")
	dispatched.mu.Unlock()

	record := logLine(logs, "record=session_connect_required")
	require.Contains(t, record, "server=agent-manager")
	require.Contains(t, record, "slack_user=U1")
	complete := logLine(logs, "record=turn_complete")
	require.Contains(t, complete, "outcome=connect_required")
	require.NotContains(t, complete, "failure_class", "a consent to give is no failure class")

	// Connected: the next start succeeds.
	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	sendConnectorInteractionURL(t, srv, "connector_connect", stateID, responseSrv.URL)
	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID) + "&server=agent-manager")
	require.NoError(t, err)
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(page), "Signed in to agent-manager")
	require.Contains(t, string(page), "Your message is on its way to the agent")

	require.Eventually(t, func() bool {
		return strings.Contains(capturedBody(), "Signed in to agent-manager")
	}, flowWait, 20*time.Millisecond, "the clicked ephemeral is replaced with the confirmation")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(updatesIn(fake, "C1")), "<@U1> connected *agent-manager*.") &&
			strings.Contains(allText(updatesIn(fake, "U1")), "Connected *agent-manager*. Your message is on its way to the agent.")
	}, flowWait, 20*time.Millisecond, "the line and the DM card are rewritten")
	require.Eventually(t, func() bool {
		dispatched.mu.Lock()
		defer dispatched.mu.Unlock()
		return len(dispatched.msgs) == 2
	}, flowWait, 20*time.Millisecond, "the held message runs once the person has connected")
	dispatched.mu.Lock()
	replayed := dispatched.msgs[1]
	dispatched.mu.Unlock()
	require.Equal(t, "how many nodes in gazelle?", replayed.Text, "the held message itself, not a synthetic continuation")
	require.Equal(t, "u1@example.com", replayed.Subject, "admitted again as the person, not as their e-mail")
	require.Equal(t, "U1", replayed.SenderID)
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), "Sign in", "no sign-in prompt: the replay knows the person")
	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "ok")
	}, flowWait, 20*time.Millisecond, "the agent's answer lands in the thread")
	require.Len(t, updatesIn(fake, "C1"), 1, "the replayed turn finds nothing left to settle")
}

// A second message in the thread before the person connects posts the
// ephemeral again, and refreshes the DM card in place instead of stacking
// cards; the line, unchanged, is left alone.
func TestSessionConnect_RepromptRefreshesTheCard(t *testing.T) {
	gw := &stubGateway{dispatchErr: sessionConnectChallenge()}
	fake, srv, _ := sessionConnectAdapter(t, gw)

	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "150.000", ""))
	require.Eventually(t, func() bool { _, _, ok := connectButton(fake); return ok },
		flowWait, 20*time.Millisecond, "the first prompt is posted")
	firstURL, _, _, ok := connectButtonIn(postsTo(fake, "U1"))
	require.True(t, ok)

	sendEvent(t, srv, mention("U1", "still there?", "151.000", "150.000"))
	require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) == 2 },
		flowWait, 20*time.Millisecond, "the second message gets the ephemeral again")
	require.Len(t, postsTo(fake, "U1"), 1, "the DM holds one card")
	require.Len(t, postsTo(fake, "C1"), 1, "the thread holds one line")

	var refreshedURL string
	require.Eventually(t, func() bool {
		refreshedURL, _, _, ok = connectButtonIn(updatesIn(fake, "U1"))
		return ok
	}, flowWait, 20*time.Millisecond, "the DM card is refreshed")
	require.NotEqual(t, stateOf(t, firstURL), stateOf(t, refreshedURL), "with the fresh completion state")
	require.Empty(t, updatesIn(fake, "C1"), "the line reads the same, so it is not rewritten")
}

// A person who connected some other way (another thread's prompt, another
// muster client) sends again: the start goes through, and the thread's line
// and the DM card settle without any landing. The held message stays unsent,
// so the card does not say it is on its way, and the stale button's landing
// runs nothing.
func TestSessionConnect_NextTurnSettlesTheSurfaces(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	fake, srv, _ := sessionConnectAdapter(t, gw)

	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "170.000", ""))
	var stateID string
	require.Eventually(t, func() bool {
		var ok bool
		_, stateID, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond, "the prompt is posted")

	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	sendEvent(t, srv, mention("U1", "connected, try again", "171.000", "170.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(updatesIn(fake, "C1")), "<@U1> connected *agent-manager*.") &&
			strings.Contains(allText(updatesIn(fake, "U1")), "Connected *agent-manager*.")
	}, flowWait, 20*time.Millisecond, "the line and the card settle on the turn that went through")
	require.NotContains(t, allText(updatesIn(fake, "U1")), "on its way", "the held message stays unsent")
	require.Eventually(t, func() bool { return strings.Contains(fake.streamedText(), "ok") },
		flowWait, 20*time.Millisecond, "the agent answers")

	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID))
	require.NoError(t, err)
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "the stale prompt's state is gone")
	require.Contains(t, string(page), "Link expired")
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 2, gw.dispatchCount(), "the held message is not run after the fact")
}

// In a direct message with the app the Connect card is a message of the
// conversation: one reader, so no ephemeral, no DM card and no line. A later
// message gets a card of its own and the earlier one loses its button. The
// card's click records no response_url; the sign-in's landing rewrites the
// card in place and runs the held message.
func TestSessionConnect_DMPostsTheCardAsAMessage(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	fake, srv := decoratedConnectorAdapter(t, gw)
	responseSrv, capturedBody := newResponseURLCapture(t)

	sendEvent(t, srv, dmEvent("U1", "how many nodes in gazelle?", "160.000"))

	var firstURL string
	var card recordedCall
	var ok bool
	require.Eventually(t, func() bool {
		firstURL, _, card, ok = connectButtonIn(fake.pathCalls("chat.postMessage"))
		return ok
	}, flowWait, 20*time.Millisecond, "the card is posted as a message")
	require.Equal(t, "D1", card.params["channel"])
	require.Contains(t, allText([]recordedCall{card}), "*agent-manager* needs you to connect your account once")
	require.NotContains(t, fmt.Sprint(card.params["blocks"]), "Asked in", "the conversation is where it was asked")
	require.Empty(t, fake.pathCalls("chat.postEphemeral"), "no ephemeral in a DM")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Waiting for", "no line in a DM")

	sendEvent(t, srv, dmThreadEvent("U1", "still there?", "161.000", "160.000"))
	require.Eventually(t, func() bool { return len(postsTo(fake, "D1")) == 2 },
		flowWait, 20*time.Millisecond, "the later message gets a card of its own under it")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(updatesIn(fake, "D1")), "Connect *agent-manager* with the newer prompt below.")
	}, flowWait, 20*time.Millisecond, "the earlier card loses its button")
	_, _, _, hasButton := connectButtonIn(updatesIn(fake, "D1"))
	require.False(t, hasButton)
	secondURL, value, _, ok := connectButtonIn(postsTo(fake, "D1")[1:])
	require.True(t, ok)
	require.Equal(t, "agent-manager", value, "the card's button carries the server: its click records no response_url")
	stateID := stateOf(t, secondURL)
	require.NotEqual(t, stateOf(t, firstURL), stateID)

	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	sendConnectorInteractionURL(t, srv, "connector_connect", value, responseSrv.URL)
	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(updatesIn(fake, "D1")), "Connected *agent-manager*. Your message is on its way to the agent.")
	}, flowWait, 20*time.Millisecond, "the card is rewritten in place")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 3 },
		flowWait, 20*time.Millisecond, "the held message runs")
	require.Empty(t, capturedBody(), "nothing is sent through the card's response_url")
}

// Without a public base URL the link cannot land back on the gateway: the
// card carries muster's own link and says to send the message again.
func TestSessionConnect_WithoutLandingSaysToResend(t *testing.T) {
	gw := &stubGateway{dispatchErr: sessionConnectChallenge()}
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
	})

	sendEvent(t, srv, dmEvent("U1", "how many nodes in gazelle?", "141.000"))

	var buttonURL, value string
	var ok bool
	require.Eventually(t, func() bool {
		buttonURL, value, _, ok = connectButtonIn(fake.pathCalls("chat.postMessage"))
		return ok
	}, flowWait, 20*time.Millisecond, "the Connect card is posted")
	require.Equal(t, sessionConnectLink, buttonURL, "muster's link as it came")
	require.Equal(t, "agent-manager", value, "no completion state: the click is a no-op")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "then send your message again")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "The turn failed")
}

// A collaborator's turn that starts the thread owner's Session meets the
// owner's challenge: the link is the owner's to follow, so the collaborator
// gets a private note naming them and no button, nothing is held, and the
// thread gets no waiting line.
func TestSessionConnect_CollaboratorIsToldTheOwnerMustConnect(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, func(a *slackadapter.Adapter) {
		a.OBO = obo
		a.ConnectorPrompts = true
		a.PublicBaseURL = "https://gw.example"
	})

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "wants to join this thread")

	gw.mu.Lock()
	gw.dispatchErr = sessionConnectChallenge()
	gw.mu.Unlock()
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "<@U001> needs to connect *agent-manager* once")
	}, flowWait, 50*time.Millisecond, "the collaborator is told the owner must connect")
	for _, c := range fake.pathCalls("chat.postEphemeral") {
		if strings.Contains(allText([]recordedCall{c}), "needs to connect") {
			require.Equal(t, "U002", c.params["user"], "the note goes to the collaborator")
		}
	}
	_, _, ok := connectButton(fake)
	require.False(t, ok, "the owner's link is not offered to the collaborator")
	_, _, _, ok = connectButtonIn(fake.pathCalls("chat.postMessage"))
	require.False(t, ok, "and goes to no DM")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Waiting for", "no line for a consent that is not the sender's")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "The turn failed")
}
