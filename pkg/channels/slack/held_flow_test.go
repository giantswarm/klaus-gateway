package slack_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// heldIn reports whether the thread's row holds state for the adapter: a
// parked or queued message, or a paused prompt.
func heldIn(t *testing.T, rec *slackadapter.MemoryRecorder, channelID, threadID string) bool {
	t.Helper()
	row, ok, err := rec.ThreadRecord(t.Context(), "slack", channelID, threadID)
	require.NoError(t, err)
	return ok && len(row.Held) > 0
}

// parkedIn reports whether the thread's row holds the given kind of held
// state: "access" or "login" for a parked message, "pending" for a paused
// prompt.
func parkedIn(t *testing.T, rec *slackadapter.MemoryRecorder, channelID, threadID, kind string) bool {
	t.Helper()
	row, ok, err := rec.ThreadRecord(t.Context(), "slack", channelID, threadID)
	require.NoError(t, err)
	return ok && strings.Contains(string(row.Held), `"`+kind+`"`)
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
	openConversationIn(t, shared, "C1", "100.000", "U123", "test-agent")
	sendEvent(t, srv1, mention("U123", "what failed on prod?", "100.000", ""))
	sendEvent(t, srv1, mention("U123", "and on staging?", "101.000", "100.000"))
	fake.waitForPath(t, "chat.postMessage", 1)
	waitThreadIdle(t, a1, "100.000")
	require.NoError(t, a1.Stop(context.Background()))
	require.True(t, parkedIn(t, shared, "C1", "100.000", "login"), "the parked messages are in the thread's row")

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
	openConversationIn(t, shared, "C1", "100.000", "U123", "test-agent")
	sendEvent(t, srv1, mention("U123", "what failed on prod?", "100.000", ""))
	fake.waitForPath(t, "chat.postMessage", 1)
	waitThreadIdle(t, a1, "100.000")
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
	gw1.openConversation(t, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv1, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw1.dispatchCount() == 1 && a1.ThreadIdle("100.000") },
		flowWait, 20*time.Millisecond, "the initiator's opening turn completes")
	sendEvent(t, srv1, mention("U999", "first ask", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 2)
	sendEvent(t, srv1, mention("U999", "second ask", "201.000", "100.000"))
	waitThreadIdle(t, a1, "100.000")
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

// queuedIn reports whether the thread's row holds a message with text in its
// inbox.
func queuedIn(t *testing.T, rec *slackadapter.MemoryRecorder, channelID, threadID, text string) bool {
	t.Helper()
	row, ok, err := rec.ThreadRecord(t.Context(), "slack", channelID, threadID)
	require.NoError(t, err)
	return ok && strings.Contains(string(row.Held), `"inbox"`) && strings.Contains(string(row.Held), text)
}

// A message acknowledged while a turn runs is in the thread's row before Slack
// has its answer, so a process torn down before it ran loses nothing: the next
// one runs it, once. The turn that was running is not run again either: the
// row records it as left running, and the restart's recovery delivers it.
func TestRestart_QueuedMessageRunsOnceAfterRestart(t *testing.T) {
	slackadapter.SetRestoredInboxGrace(t, 0)
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()
	openConversationIn(t, shared, "D1", "555.000", "U1", "test-agent")

	hold := make(chan struct{})
	defer close(hold)
	gw1 := &stubGateway{hold: hold, records: shared, resumes: &stubResumes{durable: true},
		deltas:     []channels.OutboundDelta{{Content: "looking"}},
		onDispatch: func(channels.InboundMessage) { seedInFlightRow(shared) }}
	a1, srv1 := newEventsAdapter(t, gw1, api.URL)
	sendEvent(t, srv1, dmEvent("U1", "check the nodes", "555.000"))
	require.Eventually(t, func() bool { return gw1.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)

	sendEvent(t, srv1, dmThreadEvent("U1", "and the pods?", "556.000", "555.000"))
	require.True(t, queuedIn(t, shared, "D1", "555.000", "and the pods?"), "the message is in the row when Slack has its answer")
	require.NoError(t, a1.Stop(context.Background()))
	require.Equal(t, 1, gw1.dispatchCount(), "the first process never ran it")

	row, _, err := shared.ThreadRecord(t.Context(), "slack", "D1", "555.000")
	require.NoError(t, err)
	gw2, captured := captureDispatch(shared)
	gw2.resumes = &stubResumes{
		durable: true,
		turns: []channels.InFlightTurn{{
			Msg:    channels.InboundMessage{Channel: "slack", ChannelID: "D1", ThreadID: "555.000", AgentRef: row.AgentRef, Resume: row.Resume},
			TaskID: row.TaskID,
		}},
		deltas: map[string][]channels.OutboundDelta{row.TaskID: {{Content: "the nodes are fine"}, {Done: true}}},
	}
	a2, _ := newEventsAdapter(t, gw2, api.URL)
	a2.RecoverTurns()

	require.Eventually(t, func() bool { return len(captured()) > 0 }, flowWait, 20*time.Millisecond,
		"the queued message runs after the restart")
	waitThreadIdle(t, a2, "555.000")
	require.Never(t, func() bool { return len(captured()) > 1 }, 300*time.Millisecond, 20*time.Millisecond,
		"it runs once, and the turn left running is not sent again")
	require.Equal(t, "and the pods?", captured()[0].Text)
	require.Eventually(t, func() bool { return strings.Contains(fake.streamedText(), "the nodes are fine") },
		flowWait, 20*time.Millisecond, "the recovery delivers the turn left running")
	require.False(t, heldIn(t, shared, "D1", "555.000"), "the inbox is empty")
}

// A message whose turn the agent took is not sent again by the next process,
// also when the gateway stops in the turn's last steps, after the answer
// landed: the message left the inbox when the turn went to the agent.
func TestRestart_TurnStoppedInItsTailDoesNotRunAgain(t *testing.T) {
	slackadapter.SetRestoredInboxGrace(t, 0)
	fake := newFakeSlackAPI()
	var stopping atomic.Bool
	tail := make(chan struct{})
	var once sync.Once
	fake.setDelayIf(func(path string, _ map[string]any) time.Duration {
		if path != "reactions.remove" || stopping.Load() {
			return 0
		}
		once.Do(func() { close(tail) })
		return flowWait
	})
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()
	openConversationIn(t, shared, "D1", "555.000", "U1", "test-agent")

	gw1, captured1 := captureDispatch(shared)
	gw1.deltas = []channels.OutboundDelta{{Content: "the nodes are fine"}, {Done: true}}
	a1, srv1 := newEventsAdapter(t, gw1, api.URL)
	sendEvent(t, srv1, dmEvent("U1", "check the nodes", "555.000"))
	select {
	case <-tail:
	case <-time.After(flowWait):
		t.Fatal("the turn never reached the swap of its reaction")
	}
	require.Contains(t, fake.streamedText(), "the nodes are fine", "the answer landed before the stop")
	stopping.Store(true)
	require.NoError(t, a1.Stop(context.Background()))
	require.Len(t, captured1(), 1)

	gw2, captured2 := captureDispatch(shared)
	a2, _ := newEventsAdapter(t, gw2, api.URL)
	a2.RecoverTurns()
	a2.WaitHeldRestored()
	require.Never(t, func() bool { return len(captured2()) > 0 }, 500*time.Millisecond, 20*time.Millisecond,
		"the message is not sent to the agent again")
	require.False(t, queuedIn(t, shared, "D1", "555.000", "check the nodes"))
}

// In a rolling update the old process still runs a turn, with a message queued
// behind it, when the new one starts and gets the thread's next message. The
// new one leaves the inbox it read back alone for a while, and the old one,
// once told to stop, starts no queued message: each queued message runs once,
// in order, on the new process.
func TestRestart_OverlappingProcessesRunAQueuedMessageOnce(t *testing.T) {
	slackadapter.SetRestoredInboxGrace(t, 3*time.Second)
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()
	openConversationIn(t, shared, "D1", "555.000", "U1", "test-agent")

	hold := make(chan struct{})
	gw1, captured1 := captureDispatch(shared)
	gw1.deltas, gw1.hold = []channels.OutboundDelta{{Content: "looking"}}, hold
	ctx1, signal1 := context.WithCancel(context.Background())
	_, srv1 := newEventsAdapterIn(t, ctx1, signal1, gw1, api.URL)
	sendEvent(t, srv1, dmEvent("U1", "check the nodes", "555.000"))
	require.Eventually(t, func() bool { return len(captured1()) == 1 }, flowWait, 20*time.Millisecond)
	sendEvent(t, srv1, dmThreadEvent("U1", "and the pods?", "556.000", "555.000"))

	gw2, captured2 := captureDispatch(shared)
	a2, srv2 := newEventsAdapter(t, gw2, api.URL)
	a2.RecoverTurns()
	a2.WaitHeldRestored()
	sendEvent(t, srv2, dmThreadEvent("U1", "and the logs?", "557.000", "555.000"))
	require.Never(t, func() bool { return len(captured2()) > 0 }, 500*time.Millisecond, 20*time.Millisecond,
		"the new process runs nothing while the old one may still run the inbox")

	signal1()
	close(hold)
	require.Never(t, func() bool { return len(captured1()) > 1 || len(captured2()) > 0 }, 500*time.Millisecond, 20*time.Millisecond,
		"the old process, told to stop, starts no queued message")
	require.Eventually(t, func() bool { return len(captured2()) == 2 }, flowWait, 20*time.Millisecond,
		"the new process runs the queued messages once the old one can no longer")
	got := captured2()
	require.Equal(t, []string{"and the pods?", "and the logs?"}, []string{got[0].Text, got[1].Text})
	require.Never(t, func() bool { return len(captured1()) > 1 || len(captured2()) > 2 }, 300*time.Millisecond, 20*time.Millisecond)
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
	openConversationIn(t, shared, "D1", "400.000", "U1", "test-agent")
	sendEvent(t, srv1, dmEvent("U1", "clean up", "400.000"))
	fake.waitForPath(t, "chat.postMessage", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "Approval required")
	waitThreadIdle(t, a1, "400.000")
	require.NoError(t, a1.Stop(context.Background()))
	require.True(t, parkedIn(t, shared, "D1", "400.000", "pending"), "the paused prompt is in the thread's row")

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
