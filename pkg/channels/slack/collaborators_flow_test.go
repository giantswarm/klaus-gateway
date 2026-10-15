package slack_test

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

const shareRefusal = "This Session cannot be shared: its agent runs code from each turn. Ask the others to start a Session of their own."

// recordShareRefusal binds the thread to a session and records the
// controller's refusal to share it, as the facade does on a refused
// collaborator turn.
func recordShareRefusal(t *testing.T, gw *stubGateway, threadID string) {
	t.Helper()
	require.NoError(t, gw.UpdateThreadRecord(t.Context(), "slack", "C1", threadID, func(e *store.Entry, _ bool) bool {
		e.AgentInstanceID = "session-1"
		e.ShareRefusal = &store.ShareRefusal{InstanceID: "session-1", Reason: shareRefusal}
		return true
	}))
}

// Once the controller refused to share the thread's session, a newcomer is
// refused at once with the controller's reason: the initiator is not asked
// to let them in, and nothing reaches the agent. The initiator's own turns
// run.
func TestCollaborators_NewcomerOnUnshareableSessionIsRefused(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode)

	sendEvent(t, srv, mention("U001", "fix the flaky test", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	recordShareRefusal(t, gw, "100.000")

	sendEvent(t, srv, mention("U999", "also bump the chart", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	ephemeral := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, ephemeral, shareRefusal)
	require.NotContains(t, ephemeral, "wants to join this thread", "the initiator is not asked to let them in")

	sendEvent(t, srv, mention("U001", "and the docs", "300.000", "100.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 50*time.Millisecond, "the initiator keeps instructing the agent")
	require.Len(t, fake.pathCalls("chat.postEphemeral"), 1, "the refusal goes to the newcomer once")
}

// A refusal recorded for a session the thread has since left refuses nobody:
// the newcomer is held for the initiator's consent as usual.
func TestCollaborators_RefusalOfALeftSessionIsIgnored(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode)

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	recordShareRefusal(t, gw, "100.000")
	require.NoError(t, gw.UpdateThreadRecord(t.Context(), "slack", "C1", "100.000", func(e *store.Entry, _ bool) bool {
		e.AgentInstanceID = "session-2"
		return true
	}))

	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	ephemeral := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, ephemeral, "wants to join this thread")
	require.NotContains(t, ephemeral, shareRefusal)
}

// A collaborator the thread admitted meets the controller's refusal when the
// turn asks for the thread's share: the thread is told the controller's
// reason, not a generic failure.
func TestCollaborators_GrantedCollaboratorMeetsTheControllersRefusal(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	var mu sync.Mutex
	var collaboratorTurns int
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	gw.onDispatch = func(m channels.InboundMessage) {
		if !m.Collaborator {
			return
		}
		mu.Lock()
		collaboratorTurns++
		mu.Unlock()
	}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, func(a *slackadapter.Adapter) { a.OBO = obo })

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "wants to join this thread")

	gw.mu.Lock()
	gw.dispatchErr = &pkga2a.ShareRefusedError{SessionID: "session-1", Reason: shareRefusal}
	gw.mu.Unlock()
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), shareRefusal)
	}, flowWait, 50*time.Millisecond, "the thread is told the controller's reason")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, collaboratorTurns, "the collaborator's turn went to the gateway once")
}

// A collaborator's turn the controller refuses to share is not a failure of
// the platform: the parked message replayed after the access grant and a
// later message both end as share_refused with no failure class, and are
// logged at INFO, not ERROR. The thread is told the reason once per turn.
func TestCollaborators_ShareRefusalIsNotAFailure(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	logs := &syncBuffer{}
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, func(a *slackadapter.Adapter) {
		a.OBO = obo
		a.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	refusalNotes := func() int {
		return strings.Count(allText(fake.pathCalls("chat.postMessage")), shareRefusal)
	}

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)

	gw.mu.Lock()
	gw.dispatchErr = &pkga2a.ShareRefusedError{SessionID: "session-1", Reason: shareRefusal}
	gw.mu.Unlock()
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	require.Eventually(t, func() bool { return refusalNotes() == 1 },
		flowWait, 50*time.Millisecond, "the replay tells the thread the controller's reason")
	require.Eventually(t, func() bool {
		return logLine(logs, "level=INFO", "replay after access grant failed") != ""
	}, flowWait, 50*time.Millisecond)

	sendEvent(t, srv, mention("U002", "and now?", "300.000", "100.000"))
	require.Eventually(t, func() bool { return refusalNotes() == 2 },
		flowWait, 50*time.Millisecond, "a later turn is told the reason too")
	require.Eventually(t, func() bool {
		return logLine(logs, "level=INFO", "collaborator turn refused") != ""
	}, flowWait, 50*time.Millisecond)

	require.Empty(t, logLine(logs, "level=ERROR"))
	require.Empty(t, logLine(logs, "outcome=send_failed"))
	require.Equal(t, 2, strings.Count(logs.String(), "outcome=share_refused"))
	require.NotContains(t, logLine(logs, "outcome=share_refused"), "failure_class")
	require.Equal(t, 2, refusalNotes(), "each refused turn posts one note")
}

// A Declarative agent keeps its thread's collaborators: the newcomer is held
// for the initiator's consent and, once let in, runs as a collaborator on the
// initiator's shared Session.
func TestCollaborators_DeclarativeAgentKeepsCollaborators(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	var mu sync.Mutex
	var msgs []channels.InboundMessage
	gw := &stubGateway{
		deltas:     []channels.OutboundDelta{{Content: "ok", Done: true}},
		onDispatch: func(m channels.InboundMessage) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() },
	}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, func(a *slackadapter.Adapter) { a.OBO = obo })

	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "wants to join this thread")

	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 50*time.Millisecond, "approval replays the collaborator's message")
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), shareRefusal)

	mu.Lock()
	defer mu.Unlock()
	require.True(t, msgs[1].Collaborator)
	require.Equal(t, "tok-collab", msgs[1].BearerToken)
	require.Equal(t, "tok-initiator", msgs[1].OwnerToken)
}
