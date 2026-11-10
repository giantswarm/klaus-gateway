package slack_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// The Messages tab lists an assistant-pane chat by its title. The picker's
// question is the one message that opens a conversation — in the pane the
// slash command posts it as the chat's root — and its turn must create the
// session with the title, while a later reply must not send one.
//
// The title also travels to the gateway on every turn, which is what names the
// thread's kagent conversation: Slack takes a title only when it creates the
// session, the gateway only when it creates the conversation, and a turn that
// switches agents creates one mid-thread.
func TestSessionTitle_PickerOpenerNamesTheSession(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	var titles []string
	gw := &stubGateway{
		deltas:     []channels.OutboundDelta{{Content: "ok", Done: true}},
		onDispatch: func(msg channels.InboundMessage) { mu.Lock(); titles = append(titles, msg.Title); mu.Unlock() },
	}
	api := fake.server(t)
	a, srv := newEventsAdapter(t, gw, api.URL, withSelection(pickerRoster(), pickerCards()))

	sendSlashCommand(t, srv, "D1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)
	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "why did   the CPU alert\nfire on gazelle")

	fake.waitForPath(t, "agents.sessions.setStatus", 2)
	calls := fake.pathCalls("agents.sessions.setStatus")
	require.Equal(t, "processing", calls[0].params["status"])
	require.Equal(t, "why did the CPU alert fire on gazelle", calls[0].params["title"])
	require.Equal(t, "active", calls[1].params["status"])
	require.NotContains(t, calls[1].params, "title")

	// The root the command posted is the chat, and the thread of every reply.
	thread, _ := calls[0].params["thread_ts"].(string)
	require.NotEmpty(t, thread)
	waitThreadIdle(t, a, thread)
	sendEvent(t, srv, dmThreadEvent("U1", "and the disk?", "301.000", thread))

	fake.waitForPath(t, "agents.sessions.setStatus", 4)
	calls = fake.pathCalls("agents.sessions.setStatus")
	require.Equal(t, "processing", calls[2].params["status"])
	require.NotContains(t, calls[2].params, "title", "a reply is not the opener")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"why did the CPU alert fire on gazelle", "and the disk?"}, titles,
		"every turn carries its own title for the gateway to name a conversation with")
}

// A conversation a person who has not signed in yet opens is held and replayed
// after the link. The replay re-enters dispatch without the agent the picker
// stamped, reading it from the thread's row instead; the title must survive
// the hold.
func TestSessionTitle_SurvivesSignInReplay(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	api := fake.server(t)
	a, srv := newEventsAdapter(t, gw, api.URL, withSelection(pickerRoster(), pickerCards()))
	obo := &fakeOBO{linkedUser: "U1", token: "tok", notYetLinked: true}
	a.OBO = obo

	sendSlashCommand(t, srv, "D1", "U1", "", api.URL+"/response_url")
	pm := openedView(t, fake)["private_metadata"].(string)
	sendAskAgentSubmission(t, srv, "U1", pm, "kagent/sre-agent", "why did the CPU alert fire on gazelle")
	require.Eventually(t, func() bool {
		// The command's echo is one message; the sign-in prompt the other.
		return len(fake.pathCalls("chat.postMessage"))+len(fake.pathCalls("chat.postEphemeral")) > 1
	}, flowWait, 50*time.Millisecond, "the unlinked opener gets the sign-in prompt")
	require.Zero(t, gw.dispatchCount(), "the unlinked opener must be held, not dispatched")
	require.Empty(t, fake.pathCalls("agents.sessions.setStatus"), "no turn ran, so no session was created")

	obo.completeLink()
	a.OnUserLinked(t.Context(), "U1", "u1@example.com")

	fake.waitForPath(t, "agents.sessions.setStatus", 2)
	calls := fake.pathCalls("agents.sessions.setStatus")
	require.Equal(t, "processing", calls[0].params["status"])
	require.Equal(t, "why did the CPU alert fire on gazelle", calls[0].params["title"], "the replayed opener creates the session with its title")
}
