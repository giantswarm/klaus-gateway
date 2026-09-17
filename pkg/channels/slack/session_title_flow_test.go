package slack_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// The Messages tab lists an assistant-pane chat by its title, and every
// message in that pane arrives as a reply under a Slack-created thread anchor:
// the chat's first message is never its own thread root. Its turn must still
// create the session with the title, and a later reply must not send one.
func TestSessionTitle_AssistantPaneOpenerNamesTheSession(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmThreadEvent("U1", "why did   the CPU alert\nfire on gazelle", "300.000", "100.000"))

	fake.waitForPath(t, "agents.sessions.setStatus", 2)
	calls := fake.pathCalls("agents.sessions.setStatus")
	require.Equal(t, "processing", calls[0].params["status"])
	require.Equal(t, "why did the CPU alert fire on gazelle", calls[0].params["title"])
	require.Equal(t, "active", calls[1].params["status"])
	require.NotContains(t, calls[1].params, "title")

	waitThreadIdle(t, a, "100.000")
	sendEvent(t, srv, dmThreadEvent("U1", "and the disk?", "301.000", "100.000"))

	fake.waitForPath(t, "agents.sessions.setStatus", 4)
	calls = fake.pathCalls("agents.sessions.setStatus")
	require.Equal(t, "processing", calls[2].params["status"])
	require.NotContains(t, calls[2].params, "title", "a reply is not the opener")
}

// A first chat from a user who has not signed in yet is held and replayed
// after the link. The replay re-enters dispatch with the conversation already
// bound, so it no longer reads as the opener; the title must survive the hold.
func TestSessionTitle_SurvivesSignInReplay(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	obo := &fakeOBO{linkedUser: "U1", token: "tok", notYetLinked: true}
	a.OBO = obo

	sendEvent(t, srv, dmThreadEvent("U1", "why did the CPU alert fire on gazelle", "300.000", "100.000"))
	require.Eventually(t, func() bool {
		return len(fake.pathCalls("chat.postMessage"))+len(fake.pathCalls("chat.postEphemeral")) > 0
	}, flowWait, 50*time.Millisecond, "the unlinked opener gets the sign-in prompt")
	require.Zero(t, gw.resolveCount(), "the unlinked opener must be held, not dispatched")
	require.Empty(t, fake.pathCalls("agents.sessions.setStatus"), "no turn ran, so no session was created")

	obo.completeLink()
	a.OnUserLinked(t.Context(), "U1", "u1@example.com")

	fake.waitForPath(t, "agents.sessions.setStatus", 2)
	calls := fake.pathCalls("agents.sessions.setStatus")
	require.Equal(t, "processing", calls[0].params["status"])
	require.Equal(t, "why did the CPU alert fire on gazelle", calls[0].params["title"], "the replayed opener creates the session with its title")
}
