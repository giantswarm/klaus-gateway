package slack_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

func greenPrompt(taskID string) channels.OutboundDelta {
	return channels.OutboundDelta{
		Kind:   channels.DeltaPrompt,
		TaskID: taskID,
		Prompt: &channels.HitlPrompt{ToolName: "list_pods"},
	}
}

// An agent looping on green-classified prompts is cut off at the per-turn
// auto-approval cap: the next prompt is surfaced to the human instead of
// holding the thread forever.
func TestAutoApprove_CappedPerTurn(t *testing.T) {
	fake := newFakeSlackAPI()
	var queue [][]channels.OutboundDelta
	for i := range 30 {
		queue = append(queue, []channels.OutboundDelta{greenPrompt(fmt.Sprintf("task-%d", i))})
	}
	gw := &stubGateway{sendQueue: queue}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.Classifier = slackadapter.NewClassifier("green", nil)

	sendEvent(t, srv, dmEvent("U1", "loop forever", "600.000"))

	fake.waitForPath(t, "chat.postMessage", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), "Waiting for approval",
		"past the cap the prompt is surfaced to the human")
	// 1 initial send + the capped number of auto-approved resumes, not all 30.
	require.Equal(t, 21, gw.resolveCount(), "auto-approvals stop at the cap")
}

// /stop cancels a turn that already went through an auto-approved resume; the
// thread is released and a follow-up message dispatches instead of bouncing off
// a stuck serialization slot.
func TestStop_CancelsAutoApprovedResume(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	gw := &stubGateway{
		sendQueue: [][]channels.OutboundDelta{
			{greenPrompt("task-1")},
			{{Content: "still working"}}, // no Done: the resumed segment runs until cancelled
		},
		hold: hold,
	}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.Classifier = slackadapter.NewClassifier("green", nil)

	sendEvent(t, srv, dmEvent("U1", "go", "700.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "green prompt auto-approves and the turn resumes")

	sendEvent(t, srv, dmThreadEvent("U1", "/stop", "701.000", "700.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Stopped")
	}, 2*time.Second, 50*time.Millisecond, "/stop acknowledged")

	// The resumed turn is cancelled, so the thread slot frees and a new message
	// dispatches rather than hitting the busy notice. Slot release is
	// asynchronous to the /stop ack, so keep re-sending until one gets through;
	// if the resumed segment were not cancelled the slot would never free.
	next := 702
	require.Eventually(t, func() bool {
		sendEvent(t, srv, dmThreadEvent("U1", "again", fmt.Sprintf("%d.000", next), "700.000"))
		next++
		time.Sleep(50 * time.Millisecond)
		return gw.resolveCount() >= 3
	}, 3*time.Second, 10*time.Millisecond, "post-/stop message must start a fresh turn")
}

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
	a.Classifier = slackadapter.NewClassifier("green", nil)

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
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.Classifier = slackadapter.NewClassifier("green", nil)

	sendEvent(t, srv, dmEvent("U1", "clean up", "900.000"))
	fake.waitForPath(t, "chat.postMessage", 1) // approval prompt surfaced

	gw.mu.Lock()
	gw.failSends = 1
	gw.mu.Unlock()

	sendEvent(t, srv, dmThreadEvent("U1", "approve", "901.000", "900.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 },
		2*time.Second, 50*time.Millisecond, "failed resume attempted")

	// Retry: the task must still be pending, so this reply resumes task-1 with a
	// structured decision instead of starting a fresh turn.
	sendEvent(t, srv, dmThreadEvent("U1", "approve", "902.000", "900.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "done")
	}, 2*time.Second, 50*time.Millisecond, "retried resume completes")

	mu.Lock()
	defer mu.Unlock()
	last := resolved[len(resolved)-1]
	require.Equal(t, "task-1", last.TaskID, "retry must resume the restored pending task")
	require.NotNil(t, last.Decision, "retry must carry the structured approval decision")
}
