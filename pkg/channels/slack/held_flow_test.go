package slack_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// heldIn reports whether the thread's row holds state for the adapter: a
// parked message or a paused prompt.
func heldIn(t *testing.T, rec *slackadapter.MemoryRecorder, channelID, threadID string) bool {
	t.Helper()
	row, ok, err := rec.ThreadRecord(t.Context(), "slack", channelID, threadID)
	require.NoError(t, err)
	return ok && len(row.Held) > 0
}

// captureDispatch is a stub gateway over rec that records every dispatched
// message.
func captureDispatch(rec *slackadapter.MemoryRecorder) (*stubGateway, func() []channels.InboundMessage) {
	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{records: rec, deltas: []channels.OutboundDelta{{Content: "ok", Done: true}},
		onDispatch: func(msg channels.InboundMessage) {
			mu.Lock()
			captured = append(captured, msg)
			mu.Unlock()
		}}
	return gw, func() []channels.InboundMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]channels.InboundMessage(nil), captured...)
	}
}

// A message parked while its sender signs in survives a restart between the
// sign-in prompt and the callback: the sign-in completed on the new process
// replays it (klaus-gateway#132).
func TestRestart_LoginParkedMessageReplaysAfterSignIn(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()

	a1, srv1 := newEventsAdapter(t, &stubGateway{records: shared}, api.URL, channelMode)
	a1.OBO = &fakeOBO{linkedUser: "U123", token: "tok", notYetLinked: true}
	sendEvent(t, srv1, mention("U123", "what failed on prod?", "100.000", ""))
	sendEvent(t, srv1, mention("U123", "and on staging?", "101.000", "100.000"))
	fake.waitForPath(t, "chat.postMessage", 1)
	require.NoError(t, a1.Stop(context.Background()))
	require.True(t, heldIn(t, shared, "C1", "100.000"), "the parked messages are in the thread's row")

	gw2, captured := captureDispatch(shared)
	a2, _ := newEventsAdapter(t, gw2, api.URL, channelMode)
	obo := &fakeOBO{linkedUser: "U123", token: "tok", notYetLinked: true}
	a2.OBO = obo
	a2.RecoverTurns()
	a2.WaitHeldRestored()

	obo.completeLink()
	a2.OnUserLinked(t.Context(), "U123", "u123@example.com")
	require.Eventually(t, func() bool { return len(captured()) == 2 },
		flowWait, 20*time.Millisecond, "both parked messages replay after the restart")
	got := captured()
	require.Equal(t, "what failed on prod?", got[0].Text, "replay keeps arrival order")
	require.Equal(t, "and on staging?", got[1].Text, "replay keeps arrival order")
	require.Eventually(t, func() bool { return !heldIn(t, shared, "C1", "100.000") },
		flowWait, 20*time.Millisecond, "the replayed messages leave the row")
}

// A sign-in that completed while no process held the parked message (during
// the restart) replays it as soon as the new process has read it back.
func TestRestart_SignInDuringRestartReplaysAtStart(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()

	a1, srv1 := newEventsAdapter(t, &stubGateway{records: shared}, api.URL, channelMode)
	a1.OBO = &fakeOBO{linkedUser: "U123", token: "tok", notYetLinked: true}
	sendEvent(t, srv1, mention("U123", "what failed on prod?", "100.000", ""))
	fake.waitForPath(t, "chat.postMessage", 1)
	require.NoError(t, a1.Stop(context.Background()))

	gw2, captured := captureDispatch(shared)
	a2, _ := newEventsAdapter(t, gw2, api.URL, channelMode)
	a2.OBO = &fakeOBO{linkedUser: "U123", token: "tok"}
	a2.RecoverTurns()
	require.Eventually(t, func() bool { return len(captured()) == 1 },
		flowWait, 20*time.Millisecond, "the parked message replays without a second sign-in")
	require.Equal(t, "what failed on prod?", captured()[0].Text)
}

// A newcomer's messages parked while the initiator decides survive a restart:
// the initiator's Allow on the new process replays them instead of answering
// that the request expired.
func TestRestart_AccessParkedMessagesReplayOnGrant(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()
	linked := map[string]string{"U001": "tok1", "U999": "tok9"}

	gw1 := &stubGateway{records: shared, deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	a1, srv1 := newEventsAdapter(t, gw1, api.URL, channelMode)
	a1.OBO = &multiUserOBO{linked: linked}
	sendEvent(t, srv1, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw1.dispatchCount() == 1 && a1.ThreadIdle("100.000") },
		flowWait, 20*time.Millisecond, "the initiator's opening turn completes")
	sendEvent(t, srv1, mention("U999", "first ask", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)
	sendEvent(t, srv1, mention("U999", "second ask", "201.000", "100.000"))
	require.Eventually(t, func() bool {
		row, _, err := shared.ThreadRecord(t.Context(), "slack", "C1", "100.000")
		return err == nil && strings.Contains(string(row.Held), "second ask")
	}, flowWait, 20*time.Millisecond, "both parked messages reach the thread's row")
	require.NoError(t, a1.Stop(context.Background()))

	gw2, captured := captureDispatch(shared)
	a2, srv2 := newEventsAdapter(t, gw2, api.URL, channelMode)
	a2.OBO = &multiUserOBO{linked: linked}
	a2.RecoverTurns()
	a2.WaitHeldRestored()

	sendAccessInteraction(t, srv2, "U001", accessAllowAction, "100.000", "U999", api.URL+"/response")
	require.Eventually(t, func() bool { return len(captured()) == 2 },
		flowWait, 20*time.Millisecond, "both parked messages replay on the grant after the restart")
	got := captured()
	require.Equal(t, "first ask", got[0].Text)
	require.Equal(t, "second ask", got[1].Text)
}

// A prompt a task paused on stays answerable across a restart: the Approve
// click on the new process resumes the paused task instead of reading as
// already answered.
func TestRestart_PausedPromptAnswersAfterRestart(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()
	prompt := channels.OutboundDelta{
		Kind:   channels.DeltaPrompt,
		TaskID: "task-1",
		Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"},
	}

	a1, srv1 := newEventsAdapter(t, &stubGateway{records: shared, sendQueue: [][]channels.OutboundDelta{{prompt}}}, api.URL)
	sendEvent(t, srv1, dmEvent("U1", "clean up", "400.000"))
	fake.waitForPath(t, "chat.postMessage", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "Approval required")
	require.NoError(t, a1.Stop(context.Background()))
	require.True(t, heldIn(t, shared, "D1", "400.000"), "the paused prompt is in the thread's row")

	var mu sync.Mutex
	var resumed []channels.InboundMessage
	gw2 := &stubGateway{records: shared, deltas: []channels.OutboundDelta{{Content: "deleted"}, {Done: true}},
		onDispatch: func(msg channels.InboundMessage) {
			mu.Lock()
			resumed = append(resumed, msg)
			mu.Unlock()
		}}
	a2, srv2 := newEventsAdapter(t, gw2, api.URL)
	a2.RecoverTurns()
	a2.WaitHeldRestored()

	sendInteraction(t, srv2, "hitl_approve", "400.000")
	require.Eventually(t, func() bool { return gw2.dispatchCount() == 1 },
		flowWait, 20*time.Millisecond, "the click resumes the paused task")
	mu.Lock()
	require.Equal(t, "task-1", resumed[0].TaskID, "the resume answers the task paused before the restart")
	require.NotNil(t, resumed[0].Decision, "the click is the task's decision")
	mu.Unlock()
	fake.waitForPath(t, pathStopStream, 1)
	require.Contains(t, fake.streamedText(), "deleted")
	require.NotContains(t, allText(fake.pathCalls("chat.update")), "Already answered")
	require.Eventually(t, func() bool { return !heldIn(t, shared, "D1", "400.000") },
		flowWait, 20*time.Millisecond, "the answered prompt leaves the row")
}
