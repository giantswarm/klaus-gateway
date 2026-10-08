package slack_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

const (
	mutedNote   = "Muted. I won't reply here until someone mentions me."
	unmutedNote = "Unmuted. I'll reply to messages in this thread again."
)

// mutedAt is the thread's MutedAt as its row holds it.
func mutedAt(t *testing.T, rec *slackadapter.MemoryRecorder, threadID string) string {
	t.Helper()
	row, ok, err := rec.ThreadRecord(t.Context(), "slack", "C1", threadID)
	require.NoError(t, err)
	if !ok {
		return ""
	}
	return row.MutedAt
}

// startMutedThread opens a conversation in thread 500.000 with U1 as its
// initiator, then mutes it with U1's "mute".
func startMutedThread(t *testing.T) (*fakeSlackAPI, *stubGateway, *slackadapter.Adapter, *httptest.Server, string) {
	t.Helper()
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a, srv, apiURL := startMutedThreadWith(t, fake, gw)
	return fake, gw, a, srv, apiURL
}

// startMutedThreadWith is startMutedThread over a fake and a gateway the test
// set up first.
func startMutedThreadWith(t *testing.T, fake *fakeSlackAPI, gw *stubGateway) (*slackadapter.Adapter, *httptest.Server, string) {
	t.Helper()
	api := fake.server(t)
	a, srv := newEventsAdapter(t, gw, api.URL, channelMode)

	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "500.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	sendEvent(t, srv, threadReply("U1", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), mutedNote)
	}, flowWait, 20*time.Millisecond, "the mute is confirmed in the thread")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"), "the row keeps the mute message's ts")
	require.Equal(t, 1, gw.dispatchCount(), "the word is consumed, not sent to the agent")
	return a, srv, api.URL
}

// The initiator mutes the thread. A reply that does not mention the bot then
// reaches nobody: no turn, no consent prompt for a newcomer, no note, and the
// command words without a mention are dropped with it.
func TestMute_UnmentionedRepliesAreDropped(t *testing.T) {
	fake, gw, _, srv, _ := startMutedThread(t)
	posts := len(fake.pathCalls("chat.postMessage"))
	ephemerals := len(fake.pathCalls("chat.postEphemeral"))

	sendEvent(t, srv, threadReply("U1", "I think it is the disk", "500.002", "500.000"))
	sendEvent(t, srv, threadReply("U2", "no, the network", "500.003", "500.000"))
	sendEvent(t, srv, threadReply("U1", "usage", "500.004", "500.000"))
	sendEvent(t, srv, threadReply("U1", "stop", "500.005", "500.000"))

	require.Never(t, func() bool {
		return gw.dispatchCount() > 1 || len(fake.pathCalls("chat.postMessage")) > posts ||
			len(fake.pathCalls("chat.postEphemeral")) > ephemerals
	}, 500*time.Millisecond, 20*time.Millisecond,
		"nothing reaches the agent, nothing is posted, and the newcomer gets no consent prompt")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"), "the thread stays muted")
}

// The word mute as an upload's caption is no command, so in a muted thread it
// is a reply without a mention like any other: dropped, and the mute holds.
func TestMute_CaptionIsDropped(t *testing.T) {
	_, gw, _, srv, _ := startMutedThread(t)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","subtype":"file_share","channel_type":"channel","user":"U1","text":"mute","channel":"C1","ts":"500.006","thread_ts":"500.000","files":[{"name":"graph.png","mimetype":"image/png","url_private":"https://files.slack.com/f.png","size":10}]}}`)

	require.Never(t, func() bool { return gw.dispatchCount() > 1 }, 500*time.Millisecond, 20*time.Millisecond,
		"a captioned upload without a mention does not reach the agent")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"), "and it does not end the mute")
}

// A conversation that ends after the thread lifetime takes its mute with it:
// the next mention starts the thread over, unmuted, and replies without a
// mention reach the agent again.
func TestMute_EndedConversationStartsOverUnmuted(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	rec, advance := agingRecorder(t)
	gw.records = rec
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "look at the nodes", "560.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "560.000")
	sendEvent(t, srv, threadReply("U1", "mute", "560.001", "560.000"))
	require.Eventually(t, func() bool { return mutedAt(t, rec, "560.000") == "560.001" }, flowWait, 20*time.Millisecond)

	advance(channels.DefaultThreadTTL + time.Hour)

	sendEvent(t, srv, mention("U1", "picking this back up", "560.002", "560.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "560.000")
	require.Empty(t, mutedAt(t, rec, "560.000"), "the thread starts over unmuted")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote,
		"there is no mute left to end")

	sendEvent(t, srv, threadReply("U1", "and the disk?", "560.003", "560.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 3 }, flowWait, 20*time.Millisecond,
		"a reply without a mention reaches the agent")
}

// A mention from someone allowed ends the mute: the thread is told, the turn
// runs, and replies without a mention reach the agent again after it.
func TestMute_MentionEndsTheMute(t *testing.T) {
	fake, gw, a, srv, _ := startMutedThread(t)

	sendEvent(t, srv, mention("U1", "<@UBOT> what do you make of it?", "500.010", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote)
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"), "the mute is over")

	sendEvent(t, srv, threadReply("U1", "and the disk?", "500.011", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 3 }, flowWait, 20*time.Millisecond,
		"a reply without a mention reaches the agent again")
}

// The two copies of a mention in a muted thread — the message twin first —
// run one turn: the gate lets the message copy through to the dedup claim.
func TestMute_MentionTwinsRunOneTurn(t *testing.T) {
	fake, gw, a, srv, _ := startMutedThread(t)

	sendEvent(t, srv, threadReply("U1", "<@UBOT> what do you make of it?", "500.020", "500.000"))
	sendEvent(t, srv, mention("U1", "<@UBOT> what do you make of it?", "500.020", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")
	require.Never(t, func() bool { return gw.dispatchCount() > 2 }, 500*time.Millisecond, 20*time.Millisecond,
		"the agent answers the mention once")
	require.Equal(t, 1, strings.Count(allText(fake.pathCalls("chat.postMessage")), unmutedNote))
}

// A command word after a mention is answered and runs no turn, so the thread
// stays muted.
func TestMute_MentionedCommandKeepsTheMute(t *testing.T) {
	fake, gw, _, srv, _ := startMutedThread(t)

	sendEvent(t, srv, mention("U1", "<@UBOT> help", "500.030", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Commands: ")
	}, flowWait, 20*time.Millisecond, "the command is answered")
	require.Equal(t, 1, gw.dispatchCount())
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote)
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"))
}

// A newcomer's mention in a muted thread asks the initiator first. Deny keeps
// the thread muted; Allow ends the mute and runs the newcomer's turn.
func TestMute_NewcomerMentionGoesThroughConsent(t *testing.T) {
	fake, gw, a, srv, apiURL := startMutedThread(t)

	sendEvent(t, srv, mention("U2", "<@UBOT> can I ask too?", "500.040", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "waiting for the thread owner")
	}, flowWait, 20*time.Millisecond, "the newcomer waits for the initiator")
	sendAccessInteraction(t, srv, "U1", accessDenyAction, "500.000", "U2", apiURL+"/response")
	fake.waitForPath(t, "response", 1)
	require.Never(t, func() bool { return gw.dispatchCount() > 1 }, 500*time.Millisecond, 20*time.Millisecond,
		"a denied mention does not reach the agent")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"), "a denied mention does not end the mute")

	sendEvent(t, srv, mention("U3", "<@UBOT> and me?", "500.041", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Count(allText(fake.pathCalls("chat.postEphemeral")), "waiting for the thread owner") == 2
	}, flowWait, 20*time.Millisecond)
	sendAccessInteraction(t, srv, "U1", accessAllowAction, "500.000", "U3", apiURL+"/response")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond,
		"the allowed mention runs")
	waitThreadIdle(t, a, "500.000")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote)
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"))
}

// Only the people allowed to instruct the agent can mute it; anyone else gets
// the not-permitted note and the thread keeps answering.
func TestMute_NotPermittedForANewcomer(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "look at the nodes", "510.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "510.000")

	sendEvent(t, srv, threadReply("U2", "mute", "510.001", "510.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "only the people the thread owner allowed")
	}, flowWait, 20*time.Millisecond)
	require.Empty(t, mutedAt(t, gw.rec(), "510.000"))
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), mutedNote)
}

// A second mute is told privately that the thread is already muted, and the
// mute keeps its start.
func TestMute_AlreadyMuted(t *testing.T) {
	fake, gw, _, srv, _ := startMutedThread(t)

	sendEvent(t, srv, threadReply("U1", "Mute.", "500.050", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "Already muted.")
	}, flowWait, 20*time.Millisecond)
	require.Equal(t, 1, strings.Count(allText(fake.pathCalls("chat.postMessage")), mutedNote))
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"))
}

// A mute where the agent has no conversation is told so privately, and
// writes no row: the sender does not become the thread's initiator.
func TestMute_NoConversation(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "<@UBOT> mute", "520.001", "520.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")),
			"There is no conversation with the agent in this thread to mute.")
	}, flowWait, 20*time.Millisecond)
	_, ok, err := gw.rec().ThreadRecord(t.Context(), "slack", "C1", "520.000")
	require.NoError(t, err)
	require.False(t, ok, "no row is written")
	require.Zero(t, gw.dispatchCount())
}

// In a direct message there is nobody to talk to but the agent: the word is
// a message for it.
func TestMute_InADirectMessageIsForTheAgent(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, dispatched := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "mute", "530.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	require.Equal(t, "mute", dispatched()[0].Text)
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), mutedNote)
}

// The mute is cleared before the turn is sent, so a turn that fails leaves
// the thread answering, not silent.
func TestMute_FailedTurnLeavesTheThreadUnmuted(t *testing.T) {
	_, gw, a, srv, _ := startMutedThread(t)
	gw.mu.Lock()
	gw.dispatchErr = errors.New("controller unavailable")
	gw.mu.Unlock()

	sendEvent(t, srv, mention("U1", "<@UBOT> try again", "500.060", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"))
}

// The mute is in the thread's row, so a second adapter over the same store —
// a restart — keeps it.
func TestMute_SurvivesARestart(t *testing.T) {
	fake, gw1, a1, _, apiURL := startMutedThread(t)
	require.NoError(t, a1.Stop(context.Background()))

	gw2, captured := captureDispatch(gw1.rec())
	a2, srv2 := newEventsAdapter(t, gw2, apiURL, channelMode)
	a2.RecoverTurns()
	a2.WaitHeldRestored()
	posts := len(fake.pathCalls("chat.postMessage"))

	sendEvent(t, srv2, threadReply("U1", "still the disk", "500.070", "500.000"))
	require.Never(t, func() bool {
		return len(captured()) > 0 || len(fake.pathCalls("chat.postMessage")) > posts
	}, 500*time.Millisecond, 20*time.Millisecond, "the thread is still muted after the restart")

	sendEvent(t, srv2, mention("U1", "<@UBOT> back to you", "500.071", "500.000"))
	require.Eventually(t, func() bool { return len(captured()) == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a2, "500.000")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote)
}
