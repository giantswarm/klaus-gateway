package slack_test

import (
	"fmt"
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
	}, 10*time.Second, 50*time.Millisecond, "approved turn completes")

	sendEvent(t, srv, dmThreadEvent("U1", "/usage", "802.000", "800.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "in 30 · out 10 · total 40")
	}, 10*time.Second, 50*time.Millisecond, "last turn covers both segments of the paused turn")
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
		10*time.Second, 50*time.Millisecond, "failed resume attempted")

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
	}, 10*time.Second, 100*time.Millisecond, "retried resume completes")

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
