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
	require.NotEqual(t, "agent-manager", stateID, "the button value is the completion state, not the server name")
	parsed, err := url.Parse(buttonURL)
	require.NoError(t, err)
	require.Equal(t, "abc", parsed.Query().Get("state"), "muster's own link survives decoration")
	require.Equal(t, "https://gw.example/connectors/complete?s="+stateID, parsed.Query().Get("redirect"))

	dmURL, dmState, dmCard, ok := connectButtonIn(postsTo(fake, "U1"))
	require.True(t, ok, "the same card goes to the person's DM")
	require.Equal(t, buttonURL, dmURL, "the DM card carries the same decorated link")
	require.Equal(t, stateID, dmState, "and the same completion state")
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
	}, flowWait, 20*time.Millisecond, "the clicked prompt is rewritten to the confirmation")
	require.Eventually(t, func() bool {
		updates := allText(fake.pathCalls("chat.update"))
		return strings.Contains(updates, "<@U1> connected *agent-manager*.") &&
			strings.Contains(updates, "Connected *agent-manager*. Your message is on its way to the agent.")
	}, flowWait, 20*time.Millisecond, "the line and the DM card are rewritten")
	for _, c := range fake.pathCalls("chat.update") {
		if strings.Contains(allText([]recordedCall{c}), "<@U1> connected") {
			require.Equal(t, "C1", c.params["channel"], "the line is rewritten in the thread's channel")
		}
		if strings.Contains(allText([]recordedCall{c}), "Connected *agent-manager*") {
			require.Equal(t, "U1", c.params["channel"], "the card is rewritten in the DM")
		}
	}
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
}

// A second message in the thread before the person connects posts the
// ephemeral again, and refreshes the DM card and the line in place instead
// of stacking them.
func TestSessionConnect_RepromptRefreshesTheCardAndTheLine(t *testing.T) {
	gw := &stubGateway{dispatchErr: sessionConnectChallenge()}
	fake, srv, _ := sessionConnectAdapter(t, gw)

	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "150.000", ""))
	require.Eventually(t, func() bool { _, _, ok := connectButton(fake); return ok },
		flowWait, 20*time.Millisecond, "the first prompt is posted")
	_, firstState, _, ok := connectButtonIn(postsTo(fake, "U1"))
	require.True(t, ok)

	sendEvent(t, srv, mention("U1", "still there?", "151.000", "150.000"))
	require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) == 2 },
		flowWait, 20*time.Millisecond, "the second message gets the ephemeral again")
	require.Len(t, postsTo(fake, "U1"), 1, "the DM holds one card")
	require.Len(t, postsTo(fake, "C1"), 1, "the thread holds one line")

	var updates []recordedCall
	require.Eventually(t, func() bool {
		updates = fake.pathCalls("chat.update")
		return len(updates) >= 1
	}, flowWait, 20*time.Millisecond, "the DM card is refreshed")
	_, refreshedState, card, ok := connectButtonIn(updates)
	require.True(t, ok, "the refreshed card carries the button")
	require.Equal(t, "U1", card.params["channel"])
	require.NotEqual(t, firstState, refreshedState, "with the fresh completion state")
}

// In a direct message with the app the Connect card is a message of the
// conversation: one reader, so no ephemeral, no DM card and no line. The
// sign-in's landing rewrites it and runs the held message.
func TestSessionConnect_DMPostsTheCardAsAMessage(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	fake, srv := decoratedConnectorAdapter(t, gw)

	sendEvent(t, srv, dmEvent("U1", "how many nodes in gazelle?", "160.000"))

	var stateID string
	var card recordedCall
	var ok bool
	require.Eventually(t, func() bool {
		_, stateID, card, ok = connectButtonIn(fake.pathCalls("chat.postMessage"))
		return ok
	}, flowWait, 20*time.Millisecond, "the card is posted as a message")
	require.Equal(t, "D1", card.params["channel"])
	require.Contains(t, allText([]recordedCall{card}), "*agent-manager* needs you to connect your account once")
	require.NotContains(t, fmt.Sprint(card.params["blocks"]), "Asked in", "the conversation is where it was asked")
	require.Empty(t, fake.pathCalls("chat.postEphemeral"), "no ephemeral in a DM")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Waiting for", "no line in a DM")

	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	resp, err := http.Get(srv.URL + "/connectors/complete?s=" + url.QueryEscape(stateID))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.update")), "Connected *agent-manager*. Your message is on its way to the agent.")
	}, flowWait, 20*time.Millisecond, "the card is rewritten")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 20*time.Millisecond, "the held message runs")
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
