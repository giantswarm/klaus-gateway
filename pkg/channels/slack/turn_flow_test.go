package slack_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// Usage recorded across a human-approval pause covers the whole turn: the
// pre-pause tokens travel with the pending task and the resumed segment adds to
// them, so /usage never under- or double-counts a paused turn.
func TestUsage_CarriesAcrossApprovalPause(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{sendQueue: [][]channels.OutboundDelta{
		{
			{Usage: &channels.TurnUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
			{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"}},
		},
		{
			{Usage: &channels.TurnUsage{InputTokens: 20, OutputTokens: 5, TotalTokens: 25}},
			{Content: "deleted"},
			{Done: true},
		},
	}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "clean up", "800.000"))
	fake.waitForPath(t, "chat.postMessage", 1) // approval prompt surfaced

	// Typed approval resumes the paused task in the same thread.
	waitThreadIdle(t, a, "800.000")
	sendEvent(t, srv, dmThreadEvent("U1", "approve", "801.000", "800.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "deleted")
	}, flowWait, 50*time.Millisecond, "approved turn completes")

	sendEvent(t, srv, dmThreadEvent("U1", "/usage", "802.000", "800.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "in 30 · out 10 · total 40")
	}, flowWait, 50*time.Millisecond, "last turn covers both segments of the paused turn")
	// The pause itself must not have been recorded as a separate turn: session
	// total equals the single turn.
	usageReplies := allText(fake.pathCalls("chat.postMessage"))
	require.Equal(t, 2, strings.Count(usageReplies, "in 30 · out 10 · total 40"),
		"session total matches the single turn (no double count)")
}

// A resume that fails before its stream starts re-stores the pending task, so
// the paused A2A task is not stranded and a retry still resumes it.
func TestTypedResume_FailureKeepsPendingTask(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	var dispatched []channels.InboundMessage
	gw := &stubGateway{
		onDispatch: func(msg channels.InboundMessage) {
			mu.Lock()
			dispatched = append(dispatched, msg)
			mu.Unlock()
		},
		sendQueue: [][]channels.OutboundDelta{
			{{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"}}},
			{{Content: "done"}, {Done: true}},
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "clean up", "900.000"))
	fake.waitForPath(t, "chat.postMessage", 1) // approval prompt surfaced

	gw.mu.Lock()
	gw.failSends = 1
	gw.mu.Unlock()

	// The first approval reply races the initial turn's thread-slot release; a
	// too-early one is dropped with a busy notice and never retried, so re-send
	// with fresh timestamps until one wins the slot and the resume is attempted
	// (and fails its send, per failSends). Check before sending so only one reply
	// is ever in flight: any extra sent while a prior reply holds the slot is
	// itself dropped busy, so exactly one resume fails here and the retry loop
	// below owns the successful resume.
	firstAttempt := 0
	require.Eventually(t, func() bool {
		if gw.dispatchCount() >= 2 {
			return true
		}
		firstAttempt++
		sendEvent(t, srv, dmThreadEvent("U1", "approve", fmt.Sprintf("901.%03d", firstAttempt), "900.000"))
		return false
	}, flowWait, 50*time.Millisecond, "failed resume attempted")

	// Retry: the task must still be pending, so a reply resumes task-1 with a
	// structured decision instead of starting a fresh turn. The reply races the
	// failed turn's thread-slot release (a too-early one is dropped with a busy
	// notice), so keep replying with fresh message timestamps until the resumed
	// turn's output lands. The pending task is restored before the slot frees,
	// so whichever reply wins the slot resumes it.
	attempt := 0
	require.Eventually(t, func() bool {
		attempt++
		sendEvent(t, srv, dmThreadEvent("U1", "approve", fmt.Sprintf("902.%03d", attempt), "900.000"))
		return strings.Contains(fake.streamedText(), "done")
	}, flowWait, 100*time.Millisecond, "retried resume completes")

	mu.Lock()
	defer mu.Unlock()
	retried := false
	for _, msg := range dispatched {
		if msg.TaskID == "task-1" && msg.Decision != nil && strings.HasPrefix(msg.MessageID, "902.") {
			retried = true
		}
	}
	require.True(t, retried, "a retry must resume the restored pending task with the structured approval decision")
}

// A terminal flush that dies at the prompt handoff must not lose the paused
// task: the buffered prose may be gone, but a later typed reply still resumes
// the task instead of starting a fresh one (which would leave the paused task
// dangling with an open tool call).
func TestPromptFlushFailure_KeepsPendingTask(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail("reactions.add", "missing_scope") // force text-mode progress
	fake.setFail(pathStartStream, "fatal_error")

	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{
		onDispatch: func(msg channels.InboundMessage) {
			mu.Lock()
			captured = append(captured, msg)
			mu.Unlock()
		},
		sendQueue: [][]channels.OutboundDelta{
			{
				{Content: "let me check that"},
				{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"}},
			},
			{{Done: true}},
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "delete the pod", "111.000"))

	// The buffered text is retried at the handoff (3 chat.startStream attempts);
	// the paused note still rewrites the placeholder and the prompt still posts.
	fake.waitForPath(t, pathStartStream, 3)
	fake.waitForPath(t, "chat.update", 1)

	// A typed reply must resume the paused task. Retry with fresh timestamps
	// until the thread slot has been released and one reply dispatches; extra
	// replies losing the race run as fresh turns, so any resume counts.
	resumedTask := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(captured, func(msg channels.InboundMessage) bool {
			return msg.TaskID == "task-1"
		})
	}
	seq := 0
	require.Eventually(t, func() bool {
		seq++
		sendEvent(t, srv, dmThreadEvent("U1", "approve", fmt.Sprintf("112.%03d", seq), "111.000"))
		return resumedTask()
	}, flowWait, 100*time.Millisecond,
		"a flush failure at the prompt handoff must not strand the paused task")
}

// A /stop arriving while a turn is still starting (thread slot held, turn not
// yet registered) must actually stop it: "Stopped." may not be followed by the
// turn's answer.
func TestStop_DuringTurnStartWindow(t *testing.T) {
	fake := newFakeSlackAPI()
	dispatchEntered := make(chan struct{})
	releaseDispatch := make(chan struct{})
	var once sync.Once
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "THE-ANSWER"}, {Done: true}},
		onDispatch: func(channels.InboundMessage) {
			once.Do(func() { close(dispatchEntered) })
			<-releaseDispatch
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "do something", "300.000"))
	select {
	case <-dispatchEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never reached the gateway")
	}

	// The turn holds the thread slot but has not registered a cancelable turn yet.
	sendEvent(t, srv, dmThreadEvent("U1", "/stop", "301.000", "300.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Stopped")
	}, flowWait, 50*time.Millisecond, "/stop replies")

	close(releaseDispatch)

	require.Never(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "THE-ANSWER") ||
			strings.Contains(allText(fake.pathCalls("chat.update")), "THE-ANSWER")
	}, time.Second, 100*time.Millisecond,
		"a turn confirmed as stopped must not proceed to answer")
}

// A /stop in a thread with no in-flight turn and no pending prompt must not
// claim it stopped anything.
func TestStop_IdleThreadSaysNothingRunning(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "hi", "400.000"))
	// The done reaction is the turn's last Slack call; the thread slot frees
	// right after it.
	fake.waitForPath(t, "reactions.remove", 1)
	time.Sleep(150 * time.Millisecond)

	sendEvent(t, srv, dmThreadEvent("U1", "/stop", "401.000", "400.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Nothing is running in this thread")
	}, flowWait, 50*time.Millisecond, "an idle /stop says so")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Stopped",
		"an idle /stop must not claim it stopped anything")
}

// /stop is an intentional cancel: the working reaction is cleared silently, with
// no failed reaction and no failure note.
func TestStop_CancelClearsWorkingReactionSilently(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{hold: hold}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "long task", "555.000"))
	fake.waitForPath(t, "reactions.add", 1)
	waitTurnStreaming(t, fake)

	sendEvent(t, srv, dmThreadEvent("U1", "/stop", "556.000", "555.000"))
	fake.waitForPath(t, "reactions.remove", 1)

	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.remove"), "working reaction cleared")
	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.add"), "no failed reaction after /stop")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "turn failed",
		"no failure note for an intentional stop")
}

// An attachment-only reply into a thread with a paused confirmation must not
// become a decision (empty text would silently reject a generic prompt or
// blank-approve an ask_user one): the task stays pending, the user is asked
// for a text reply, and a later typed approval still resumes the original task.
func TestAttachmentOnlyReply_LeavesPendingTaskAndAsksForText(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	var dispatched []channels.InboundMessage
	gw := &stubGateway{
		onDispatch: func(msg channels.InboundMessage) {
			mu.Lock()
			dispatched = append(dispatched, msg)
			mu.Unlock()
		},
		sendQueue: [][]channels.OutboundDelta{
			{{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"}}},
			{{Content: "done"}, {Done: true}},
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "clean up", "910.000"))
	fake.waitForPath(t, "chat.postMessage", 1) // approval prompt surfaced

	// The reply races the initial turn's thread-slot release (a too-early one
	// bounces busy), so re-send with fresh timestamps until the needs-text note
	// lands.
	attempt := 0
	require.Eventually(t, func() bool {
		attempt++
		sendEvent(t, srv, dmThreadFileEvent("U1", "", fmt.Sprintf("911.%03d", attempt), "910.000", "shot.png"))
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "waits for the confirmation above")
	}, flowWait, 100*time.Millisecond, "needs-text note posted")

	// The task must still be pending: a typed approval resumes task-1.
	attempt = 0
	require.Eventually(t, func() bool {
		attempt++
		sendEvent(t, srv, dmThreadEvent("U1", "approve", fmt.Sprintf("912.%03d", attempt), "910.000"))
		return strings.Contains(fake.streamedText(), "done")
	}, flowWait, 100*time.Millisecond, "typed approval still resumes the task")

	mu.Lock()
	defer mu.Unlock()
	resumed := false
	for _, msg := range dispatched {
		require.False(t, strings.HasPrefix(msg.MessageID, "911."),
			"the attachment-only reply must never reach the gateway as a turn")
		if msg.TaskID == "task-1" && msg.Decision != nil && msg.Decision.Type == channels.DecisionApprove {
			resumed = true
		}
	}
	require.True(t, resumed, "the pending task resumes with the structured approval")
}

// A file riding on a typed confirmation reply is not forwarded — the reply
// carries only the decision — and the user is told so instead of the file
// silently vanishing.
func TestDecisionReplyWithAttachment_PostsNotForwardedNote(t *testing.T) {
	fake := newFakeSlackAPI()
	var mu sync.Mutex
	var dispatched []channels.InboundMessage
	gw := &stubGateway{
		onDispatch: func(msg channels.InboundMessage) {
			mu.Lock()
			dispatched = append(dispatched, msg)
			mu.Unlock()
		},
		sendQueue: [][]channels.OutboundDelta{
			{{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"}}},
			{{Content: "done"}, {Done: true}},
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "clean up", "920.000"))
	fake.waitForPath(t, "chat.postMessage", 1) // approval prompt surfaced

	attempt := 0
	require.Eventually(t, func() bool {
		attempt++
		sendEvent(t, srv, dmThreadFileEvent("U1", "approve", fmt.Sprintf("921.%03d", attempt), "920.000", "error.log"))
		return strings.Contains(fake.streamedText(), "done")
	}, flowWait, 100*time.Millisecond, "decision reply completes")

	posts := allText(fake.pathCalls("chat.postMessage"))
	require.Contains(t, posts, "carries only your decision", "the not-forwarded note is posted")
	require.Contains(t, posts, "error.log", "the note names the file")

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, msg := range dispatched {
		if msg.TaskID == "task-1" && msg.Decision != nil {
			found = true
			require.Empty(t, msg.Attachments, "no attachment travels with a decision")
		}
	}
	require.True(t, found, "the decision resumes the pending task")
}

// A bare "stop" while a turn runs is the natural reply in a thread the bot
// answers in without a mention; it interrupts the turn like /stop instead of
// being bounced with the busy notice.
func TestStop_BareWordDuringTurnInterrupts(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{hold: hold}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "long task", "700.000"))
	fake.waitForPath(t, "reactions.add", 1)
	waitTurnStreaming(t, fake)

	sendEvent(t, srv, dmThreadEvent("U1", "Stop.", "701.000", "700.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Stopped")
	}, flowWait, 50*time.Millisecond, "a bare stop replies like /stop")
	fake.waitForPath(t, "reactions.remove", 1)

	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Still answering",
		"a bare stop is not bounced as busy")
	require.Equal(t, 1, gw.dispatchCount(), "a bare stop never reaches the agent as a turn")
}

// A bare "stop" in a thread with no running turn keeps today's behaviour: it
// is a message for the agent, neither a stop nor a busy notice.
func TestStop_BareWordIdleThreadReachesAgent(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "done"}, {Done: true}}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "hi", "800.000"))
	fake.waitForPath(t, "reactions.remove", 1)
	time.Sleep(150 * time.Millisecond)

	sendEvent(t, srv, dmThreadEvent("U1", "stop", "801.000", "800.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 50*time.Millisecond,
		"an idle bare stop is dispatched to the agent")
	fake.waitForPath(t, "reactions.remove", 2)

	posted := allText(fake.pathCalls("chat.postMessage"))
	require.NotContains(t, posted, "Stopped", "nothing to stop")
	require.NotContains(t, posted, "Nothing is running", "the nothing-running notice is /stop's alone")
	require.NotContains(t, posted, "Still answering", "an idle thread is not busy")
}

// A DM turn renders its answer as one streamed message: opened on the first
// text, appended to while the agent keeps writing, closed when the turn ends —
// and the session's exit status rides that close. A DM stream names no
// recipient; Slack refuses one there.
func TestStreamedReply_DMTurnStartsAppendsAndStops(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Content: "first half "},
			{Content: "second half "},
			{Done: true},
		},
		// Past the append tick, so the second delta rides an append of its own.
		interDeltaDelay: 1200 * time.Millisecond,
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "how many nodes?", "900.000"))
	require.Eventually(t, func() bool {
		return len(fake.pathCalls(pathStopStream)) > 0
	}, flowWait, 20*time.Millisecond, "the stream is closed when the turn ends")
	require.Contains(t, fake.streamedText(), "first half second half", "the whole answer is streamed")

	require.Equal(t, []string{pathStartStream, pathAppendStream, pathStopStream}, fake.streamMethods())
	start := fake.pathCalls(pathStartStream)[0]
	require.Equal(t, "900.000", start.params["thread_ts"], "the answer streams into the thread")
	require.NotContains(t, start.params, "recipient_user_id", "a DM stream names no recipient")
	require.NotContains(t, start.params, "recipient_team_id")
	require.Equal(t, "active", fake.pathCalls(pathStopStream)[0].params["session_status"],
		"the session's exit status rides the stop")
}

// The same in a channel thread, where Slack requires the stream to name the
// person it answers.
func TestStreamedReply_ChannelTurnNamesTheRecipient(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Content: "first half "},
			{Content: "second half "},
			{Done: true},
		},
		interDeltaDelay: 1200 * time.Millisecond,
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"app_mention","user":"U1","text":"<@UBOT> how many nodes?","channel":"C1","ts":"901.000"}}`)
	// The exit status call is made after the stop that closed the stream, so
	// waiting on the stop alone races the assertion below against it.
	require.Eventually(t, func() bool {
		return len(fake.pathCalls(pathStopStream)) > 0 && len(fake.pathCalls("agents.sessions.setStatus")) == 2
	}, flowWait, 20*time.Millisecond, "the stream is closed and the session left processing when the turn ends")
	require.Contains(t, fake.streamedText(), "first half second half", "the whole answer is streamed")

	require.Equal(t, []string{pathStartStream, pathAppendStream, pathStopStream}, fake.streamMethods())
	start := fake.pathCalls(pathStartStream)[0]
	require.Equal(t, "901.000", start.params["thread_ts"])
	require.Equal(t, "U1", start.params["recipient_user_id"], "the stream names the asker")
	require.Equal(t, "TWORKSPACE", start.params["recipient_team_id"], "and their workspace")
	require.Equal(t, "active", fake.pathCalls(pathStopStream)[0].params["session_status"])

	// The stop names the exit status, but the status call is what clears the
	// working indicator, so the turn always makes it — after the stop.
	status := fake.pathCalls("agents.sessions.setStatus")
	require.Len(t, status, 2, "processing on the way in, the exit status on the way out")
	require.Equal(t, "active", status[1].params["status"])
	require.Less(t, callIndex(fake, pathStopStream), callIndex(fake, "agents.sessions.setStatus", "active"),
		"the status call follows the stop that closed the stream")
}

// callIndex is the position of the first call to path in the fake's record,
// optionally the first one whose status param is want.
func callIndex(f *fakeSlackAPI, path string, want ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c.path != path {
			continue
		}
		if len(want) > 0 {
			if s, _ := c.params["status"].(string); s != want[0] {
				continue
			}
		}
		return i
	}
	return -1
}

// Slack closing the answer's stream twice is a rendering failure, not a silent
// stop: the turn opens one replacement stream, then tells the thread the reply
// is incomplete, marks the triggering message failed, and clears the working
// indicator with its own status call.
func TestStreamedReply_StreamLostTwiceReportsTheReplyCutShort(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail(pathAppendStream, "message_not_in_streaming_state")
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{
			{Content: "first third "},
			{Content: "second third "},
			{Content: "last third "},
			{Done: true},
		},
		// Past the append tick, so each delta rides an append of its own.
		interDeltaDelay: 1200 * time.Millisecond,
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "how many nodes?", "930.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Slack refused the rest of the reply")
	}, flowWait, 20*time.Millisecond, "the thread is told the reply is incomplete")

	require.Len(t, fake.pathCalls(pathStartStream), 2, "one replacement stream, then the turn gives up")
	require.Contains(t, fake.reactionNames("reactions.add"), "x", "the triggering message carries the failed reaction")
	var statuses []string
	for _, c := range fake.pathCalls("agents.sessions.setStatus") {
		if v, ok := c.params["status"].(string); ok {
			statuses = append(statuses, v)
		}
	}
	require.Equal(t, []string{"processing", "active"}, statuses,
		"no stop carried the exit status, so the turn's own call clears the indicator")
}
