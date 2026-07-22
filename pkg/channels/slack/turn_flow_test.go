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
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "clean up", "800.000"))
	fake.waitForPath(t, "chat.postMessage", 1) // approval prompt surfaced

	// Typed approval resumes the paused task in the same thread.
	sendEvent(t, srv, dmThreadEvent("U1", "approve", "801.000", "800.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "deleted")
	}, 2*time.Second, 50*time.Millisecond, "approved turn completes")

	sendEvent(t, srv, dmThreadEvent("U1", "/usage", "802.000", "800.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "in 30 · out 10 · total 40")
	}, 2*time.Second, 50*time.Millisecond, "last turn covers both segments of the paused turn")
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
	var resolved []channels.InboundMessage
	gw := &stubGateway{
		onResolve: func(msg channels.InboundMessage) {
			mu.Lock()
			resolved = append(resolved, msg)
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

	sendEvent(t, srv, dmThreadEvent("U1", "approve", "901.000", "900.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "failed resume attempted")

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
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "done")
	}, 5*time.Second, 100*time.Millisecond, "retried resume completes")

	mu.Lock()
	defer mu.Unlock()
	retried := false
	for _, msg := range resolved {
		if msg.TaskID == "task-1" && msg.Decision != nil && msg.MessageID != "901.000" {
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
	fake.setFail("chat.update", "fatal_error")

	var mu sync.Mutex
	var captured []channels.InboundMessage
	gw := &stubGateway{
		onResolve: func(msg channels.InboundMessage) {
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

	// The buffered text is retried at the handoff (3 chat.update attempts), then
	// the paused-note rewrite makes the 4th; the prompt itself posts regardless.
	fake.waitForPath(t, "chat.update", 4)

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
	}, 3*time.Second, 100*time.Millisecond,
		"a flush failure at the prompt handoff must not strand the paused task")
}

// A /stop arriving while a turn is still starting (thread slot held, turn not
// yet registered) must actually stop it: "Stopped." may not be followed by the
// turn's answer.
func TestStop_DuringTurnStartWindow(t *testing.T) {
	fake := newFakeSlackAPI()
	resolveEntered := make(chan struct{})
	releaseResolve := make(chan struct{})
	var once sync.Once
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "THE-ANSWER"}, {Done: true}},
		onResolve: func(channels.InboundMessage) {
			once.Do(func() { close(resolveEntered) })
			<-releaseResolve
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "do something", "300.000"))
	select {
	case <-resolveEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never reached Resolve")
	}

	// The turn holds the thread slot but has not registered a cancelable turn yet.
	sendEvent(t, srv, dmThreadEvent("U1", "/stop", "301.000", "300.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Stopped")
	}, 2*time.Second, 50*time.Millisecond, "/stop replies")

	close(releaseResolve)

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
	}, 2*time.Second, 50*time.Millisecond, "an idle /stop says so")
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

	sendEvent(t, srv, dmThreadEvent("U1", "/stop", "556.000", "555.000"))
	fake.waitForPath(t, "reactions.remove", 1)

	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.remove"), "working reaction cleared")
	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.add"), "no failed reaction after /stop")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "turn failed",
		"no failure note for an intentional stop")
}
