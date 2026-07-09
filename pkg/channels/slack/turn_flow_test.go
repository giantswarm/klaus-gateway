package slack_test

import (
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
