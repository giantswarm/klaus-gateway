package slack_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

const parkedDroppedNote = "The thread was muted; mention the agent to ask again."

// parkForConsent opens a conversation in thread 600.000 with U1 as its
// initiator, then parks U2's mention for U1's consent.
func parkForConsent(t *testing.T, fake *fakeSlackAPI, gw *stubGateway) (*slackadapter.Adapter, *httptest.Server, string) {
	t.Helper()
	api := fake.server(t)
	a, srv := newEventsAdapter(t, gw, api.URL, channelMode)
	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "600.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "600.000")

	sendEvent(t, srv, mention("U2", "<@UBOT> can I ask too?", "600.010", "600.000"))
	require.Eventually(t, func() bool { return heldIn(t, gw.rec(), "C1", "600.000") },
		flowWait, 20*time.Millisecond, "the newcomer's mention is parked in the row")
	return a, srv, api.URL
}

// muteAndDrop sends U1's mute in thread 600.000 and waits until the parked
// messages are gone from the row and their sender was told.
func muteAndDrop(t *testing.T, fake *fakeSlackAPI, gw *stubGateway, srv *httptest.Server, sender string) {
	t.Helper()
	sendEvent(t, srv, threadReply("U1", "mute", "600.020", "600.000"))
	require.Eventually(t, func() bool { return ephemeralTo(fake, sender, parkedDroppedNote) },
		flowWait, 20*time.Millisecond, "the waiting sender is told privately")
	require.False(t, heldIn(t, gw.rec(), "C1", "600.000"),
		"the drop is in the row before the sender is told, not written in the background")
	require.Equal(t, "600.020", mutedAt(t, gw.rec(), "600.000"))
}

// A mention parked for consent is dropped by the mute. The consent prompt
// stays: Allow grants the newcomer, runs nothing, and the thread stays muted.
func TestMuteParked_ConsentMessageIsDropped(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a, srv, apiURL := parkForConsent(t, fake, gw)

	muteAndDrop(t, fake, gw, srv, "U2")
	require.False(t, ephemeralTo(fake, "U1", parkedDroppedNote), "only the waiting sender is told")

	sendAccessInteraction(t, srv, "U1", accessAllowAction, "600.000", "U2", apiURL+"/response")
	fake.waitForPath(t, "response", 1)
	require.Contains(t, allText(fake.pathCalls("response")), "<@U2> allowed.")
	require.Never(t, func() bool { return gw.dispatchCount() > 1 }, 500*time.Millisecond, 20*time.Millisecond,
		"the Allow runs nothing")
	require.Equal(t, "600.020", mutedAt(t, gw.rec(), "600.000"), "the thread stays muted")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote)

	// The grant stands: U2's next mention runs without a prompt and ends the mute.
	sendEvent(t, srv, mention("U2", "<@UBOT> now?", "600.030", "600.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "600.000")
	require.Empty(t, mutedAt(t, gw.rec(), "600.000"))
}

// Someone who may not mute drops nothing: the parked mention still waits for
// the initiator, and Allow runs it.
func TestMuteParked_NotPermittedDropsNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	_, srv, apiURL := parkForConsent(t, fake, gw)

	sendEvent(t, srv, threadReply("U3", "mute", "600.020", "600.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "only the people the thread owner allowed")
	}, flowWait, 20*time.Millisecond)
	require.False(t, ephemeralTo(fake, "U2", parkedDroppedNote))
	require.True(t, heldIn(t, gw.rec(), "C1", "600.000"))

	sendAccessInteraction(t, srv, "U1", accessAllowAction, "600.000", "U2", apiURL+"/response")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond,
		"the parked mention runs on the Allow")
}

// parkForSignIn opens a conversation in thread 600.000 with U1 as its
// initiator and grants U2, who is not signed in: U2's mention is parked for
// their sign-in.
func parkForSignIn(t *testing.T, fake *fakeSlackAPI, gw *stubGateway, obo *multiUserOBO) (*slackadapter.Adapter, *httptest.Server) {
	t.Helper()
	api := fake.server(t)
	a, srv := newEventsAdapter(t, gw, api.URL, channelMode)
	a.OBO = obo
	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "600.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "600.000")

	sendEvent(t, srv, mention("U2", "<@UBOT> can I ask too?", "600.010", "600.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	sendAccessInteraction(t, srv, "U1", accessAllowAction, "600.000", "U2", api.URL+"/response")
	require.Eventually(t, func() bool { return signInPrompted(fake) }, flowWait, 20*time.Millisecond,
		"the allowed newcomer is asked to sign in")
	require.Eventually(t, func() bool {
		row, _, err := gw.rec().ThreadRecord(t.Context(), "slack", "C1", "600.000")
		return err == nil && strings.Contains(string(row.Held), `"login"`)
	}, flowWait, 20*time.Millisecond, "the mention is parked for sign-in in the row")
	require.Equal(t, 1, gw.dispatchCount())
	return a, srv
}

// A mention parked for its sender's sign-in is dropped by the mute: the
// sign-in that completes later replays nothing, and the thread stays muted.
func TestMuteParked_SignInMessageIsDropped(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	obo := &multiUserOBO{linked: map[string]string{"U1": "tok1"}}
	a, srv := parkForSignIn(t, fake, gw, obo)

	muteAndDrop(t, fake, gw, srv, "U2")

	obo.link("U2", "tok2")
	a.OnUserLinked(t.Context(), "U2", "u2@example.com")
	require.Never(t, func() bool { return gw.dispatchCount() > 1 }, 500*time.Millisecond, 20*time.Millisecond,
		"the completed sign-in replays nothing")
	require.Equal(t, "600.020", mutedAt(t, gw.rec(), "600.000"))
}

// The drop is in the row: a restart replays nothing, also for a sender whose
// sign-in completed meanwhile, and the thread stays muted.
func TestMuteParked_RestartReplaysNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	gw1 := &stubGateway{records: slackadapter.NewMemoryRecorder(), deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a1, srv1 := parkForSignIn(t, fake, gw1, &multiUserOBO{linked: map[string]string{"U1": "tok1"}})
	muteAndDrop(t, fake, gw1, srv1, "U2")
	require.NoError(t, a1.Stop(context.Background()))

	gw2, captured := captureDispatch(gw1.rec())
	a2, _ := newEventsAdapter(t, gw2, fake.server(t).URL, channelMode)
	a2.OBO = &multiUserOBO{linked: map[string]string{"U1": "tok1", "U2": "tok2"}}
	a2.RecoverTurns()
	a2.WaitHeldRestored()
	require.Never(t, func() bool { return len(captured()) > 0 }, 500*time.Millisecond, 20*time.Millisecond,
		"the dropped message is not replayed after the restart")
	require.Equal(t, "600.020", mutedAt(t, gw1.rec(), "600.000"))
}

// A message restored from the row after a restart is dropped the same way.
func TestMuteParked_RestoredMessageIsDropped(t *testing.T) {
	fake := newFakeSlackAPI()
	gw1 := &stubGateway{records: slackadapter.NewMemoryRecorder(), deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a1, _, _ := parkForConsent(t, fake, gw1)
	require.NoError(t, a1.Stop(context.Background()))

	gw2 := &stubGateway{records: gw1.rec(), deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	api := fake.server(t)
	a2, srv2 := newEventsAdapter(t, gw2, api.URL, channelMode)
	a2.RecoverTurns()
	a2.WaitHeldRestored()

	muteAndDrop(t, fake, gw2, srv2, "U2")
	sendAccessInteraction(t, srv2, "U1", accessAllowAction, "600.000", "U2", api.URL+"/response")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("response")), "<@U2> allowed.")
	}, flowWait, 20*time.Millisecond)
	require.Never(t, func() bool { return gw2.dispatchCount() > 0 }, 500*time.Millisecond, 20*time.Millisecond,
		"the restored message is not replayed on the Allow")
}

// A sender with messages in both queues — one waiting for consent, one, after
// a logout, waiting for sign-in — is told once.
func TestMuteParked_EachSenderIsToldOnce(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	obo := &multiUserOBO{linked: map[string]string{"U1": "tok1", "U2": "tok2"}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
	a.OBO = obo
	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "600.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "600.000")

	sendEvent(t, srv, mention("U2", "<@UBOT> can I ask too?", "600.010", "600.000"))
	require.Eventually(t, func() bool { return heldIn(t, gw.rec(), "C1", "600.000") }, flowWait, 20*time.Millisecond)
	obo.mu.Lock()
	delete(obo.linked, "U2")
	obo.mu.Unlock()
	sendEvent(t, srv, mention("U2", "<@UBOT> hello?", "600.011", "600.000"))
	require.Eventually(t, func() bool {
		row, _, err := gw.rec().ThreadRecord(t.Context(), "slack", "C1", "600.000")
		return err == nil && strings.Contains(string(row.Held), `"login"`) && strings.Contains(string(row.Held), `"access"`)
	}, flowWait, 20*time.Millisecond, "U2 waits in both queues")

	muteAndDrop(t, fake, gw, srv, "U2")
	require.Never(t, func() bool { return droppedNotesTo(fake, "U2") != 1 },
		300*time.Millisecond, 20*time.Millisecond, "one note per sender")
}

// droppedNotesTo counts the parked-dropped notes sent to user.
func droppedNotesTo(fake *fakeSlackAPI, user string) int {
	n := 0
	for _, c := range fake.pathCalls("chat.postEphemeral") {
		if c.params["user"] == user && strings.Contains(allText([]recordedCall{c}), parkedDroppedNote) {
			n++
		}
	}
	return n
}

// An Allow taken while a turn runs leaves the newcomer's messages waiting for
// the thread's slot. A mute that stops the turn frees the slot, and the
// messages, written before the mute, run nothing; their sender is told once.
func TestMuteParked_ReplayWaitingForTheSlotIsDropped(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "thinking"}}, hold: hold}
	api := fake.server(t)
	a, srv := newEventsAdapter(t, gw, api.URL, channelMode)
	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "600.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)

	sendEvent(t, srv, mention("U2", "<@UBOT> can I ask too?", "600.010", "600.000"))
	sendEvent(t, srv, mention("U2", "<@UBOT> hello?", "600.011", "600.000"))
	require.Eventually(t, func() bool {
		row, _, err := gw.rec().ThreadRecord(t.Context(), "slack", "C1", "600.000")
		return err == nil && strings.Contains(string(row.Held), "hello?")
	}, flowWait, 20*time.Millisecond, "both mentions are parked")
	sendAccessInteraction(t, srv, "U1", accessAllowAction, "600.000", "U2", api.URL+"/response")
	require.Eventually(t, func() bool { return !heldIn(t, gw.rec(), "C1", "600.000") },
		flowWait, 20*time.Millisecond, "the Allow took the messages; their replay waits for the slot")

	sendEvent(t, srv, threadReply("U1", "mute", "600.020", "600.000"))
	require.Eventually(t, func() bool { return ephemeralTo(fake, "U2", parkedDroppedNote) },
		flowWait, 20*time.Millisecond, "the waiting sender is told")
	waitThreadIdle(t, a, "600.000")
	require.Never(t, func() bool { return gw.dispatchCount() > 1 || droppedNotesTo(fake, "U2") != 1 },
		500*time.Millisecond, 20*time.Millisecond, "the waiting replays run nothing, and the sender is told once")
	require.Equal(t, "600.020", mutedAt(t, gw.rec(), "600.000"))
}
