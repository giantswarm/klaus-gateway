package slack_test

import (
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

// sessionConnectAdapter is decoratedConnectorAdapter with the adapter's log
// captured and a users.info that answers the person's e-mail, so the turn
// rewrites the sender the way it does on an installation.
func sessionConnectAdapter(t *testing.T, gw *stubGateway) (*fakeSlackAPI, *httptest.Server, *syncBuffer) {
	t.Helper()
	fake := newFakeSlackAPI()
	fake.setResponse("users.info", `{"ok":true,"user":{"id":"U1","profile":{"email":"u1@example.com"}}}`)
	logs := &syncBuffer{}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
		a.ConnectorPrompts = true
		a.PublicBaseURL = "https://gw.example"
		a.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
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

// A Session start muster refuses with its sign-in challenge for a server gets
// the Connect prompt for that server, to the person alone, and no failure
// note. The message is held as admitted: the sign-in's landing on the gateway
// runs it as the thread's first turn, with nothing retyped and no synthetic
// continuation, also when the turn had resolved the sender to their e-mail.
func TestSessionConnect_PromptsAndRunsTheHeldMessageAfterSignIn(t *testing.T) {
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

	sendEvent(t, srv, dmEvent("U1", "how many nodes in gazelle?", "140.000"))

	var buttonURL, stateID string
	var ok bool
	require.Eventually(t, func() bool {
		buttonURL, stateID, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond, "the Connect prompt is posted")
	ephemeral := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, ephemeral, "*agent-manager* needs you to connect your account once")
	require.Contains(t, ephemeral, "your message is sent to the agent")
	require.NotEqual(t, "agent-manager", stateID, "the button value is the completion state, not the server name")
	parsed, err := url.Parse(buttonURL)
	require.NoError(t, err)
	require.Equal(t, "abc", parsed.Query().Get("state"), "muster's own link survives decoration")
	require.Equal(t, "https://gw.example/connectors/complete?s="+stateID, parsed.Query().Get("redirect"))
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "The turn failed", "a consent to give is not a failure")
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

	require.Eventually(t, func() bool {
		return strings.Contains(capturedBody(), "Signed in to agent-manager")
	}, flowWait, 20*time.Millisecond, "the prompt is rewritten to the confirmation")
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

// Without a public base URL the link cannot land back on the gateway: the
// prompt carries muster's own link and says to send the message again.
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
		buttonURL, value, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond, "the Connect prompt is posted")
	require.Equal(t, sessionConnectLink, buttonURL, "muster's link as it came")
	require.Equal(t, "agent-manager", value, "no completion state: the click is a no-op")
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "then send your message again")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "The turn failed")
}

// A collaborator's turn that starts the thread owner's Session meets the
// owner's challenge: the link is the owner's to follow, so the collaborator
// gets a private note naming them and no button, and nothing is held.
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
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "The turn failed")
}
