package slack_test

import (
	"io"
	"net/http"
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

// A Session start muster refuses with its sign-in challenge for a server gets
// the Connect prompt for that server, to the person alone, and no failure
// note. The message is held: the sign-in's landing on the gateway runs it as
// the thread's first turn, with nothing retyped and no synthetic
// continuation.
func TestSessionConnect_PromptsAndRunsTheHeldMessageAfterSignIn(t *testing.T) {
	var dispatched struct {
		mu    sync.Mutex
		texts []string
	}
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	gw.onDispatch = func(msg channels.InboundMessage) {
		dispatched.mu.Lock()
		dispatched.texts = append(dispatched.texts, msg.Text)
		dispatched.mu.Unlock()
	}
	fake, srv := decoratedConnectorAdapter(t, gw)
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
		return len(dispatched.texts) == 2
	}, flowWait, 20*time.Millisecond, "the held message runs once the person has connected")
	dispatched.mu.Lock()
	require.Equal(t, []string{"how many nodes in gazelle?", "how many nodes in gazelle?"}, dispatched.texts, "the held message itself, not a synthetic continuation")
	dispatched.mu.Unlock()
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
