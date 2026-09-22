package slack_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// callCount returns how many Slack Web API calls the fake has recorded.
func (f *fakeSlackAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// callsSince returns the calls recorded after the first n.
func (f *fakeSlackAPI) callsSince(n int) []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls[n:]...)
}

// textOf is everything the given calls carried: the streamed answer text plus
// the fallback text and blocks of the posts, so an assertion can read a window
// of the conversation whichever way the renderer delivered it. The streamed
// pieces are joined without a separator — one answer arrives in as many pieces
// as the stream sent it in.
func textOf(calls []recordedCall) string {
	var b strings.Builder
	for _, c := range calls {
		b.WriteString(chunkText(c.params))
		b.WriteString(allBlockText([]recordedCall{c}))
	}
	return b.String()
}

// callsCarrying counts the calls whose text, blocks or streamed text contain
// sub (one post carries its text twice, as the fallback text and in the block).
func callsCarrying(calls []recordedCall, sub string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(textOf([]recordedCall{c}), sub) {
			n++
		}
	}
	return n
}

func toolStep(name string) channels.OutboundDelta {
	return channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Kind: channels.ToolCall, Name: name}}
}

// seedInFlightRow records the task in flight on DM thread 555.000, as the
// facade does before the first delta reaches the adapter; the stub gateway has
// no facade, so the test writes the row the adapter's records build on.
func seedInFlightRow(t *testing.T, rec *slackadapter.MemoryRecorder) {
	t.Helper()
	require.NoError(t, rec.UpdateThreadRecord(t.Context(), "slack", "D1", "555.000", func(e *store.Entry, _ bool) bool {
		e.AgentRef, e.AgentInstanceID, e.TaskID = "test-agent", "inst-1", "task-9"
		e.Resume = map[string]string{"slack_user": "U1", "message_ts": "555.000"}
		return true
	}))
}

// A restart in the middle of a turn: the first process streams the opening of
// the answer and two tool steps and is shut down; a second process over the
// same routing store resubscribes and is handed one more step and the whole
// answer. The thread reads the opening once, the notice, then the tail — and
// the step after the restart is numbered three, not one (klaus-gateway#301).
func TestRestart_ContinuedTurnPostsOnlyWhatFollows(t *testing.T) {
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()
	seedInFlightRow(t, shared)
	const opening = "Created. One thing to flag: the repository is private.\n\n"
	const tail = "Next: rotate the deploy token."

	hold := make(chan struct{})
	defer close(hold)
	gw1 := &stubGateway{hold: hold, records: shared, resumes: &stubResumes{durable: true},
		deltas: []channels.OutboundDelta{{Content: opening}, toolStep("create"), toolStep("protect")}}
	a1, srv1 := newEventsAdapter(t, gw1, api.URL)
	sendEvent(t, srv1, dmEvent("U1", "create the repository", "555.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "One thing to flag") && len(fake.streamedSteps()) == 2
	}, flowWait, 20*time.Millisecond, "the opening and the two steps landed before the restart")
	require.NoError(t, a1.Stop(context.Background()))
	require.Contains(t, allBlockText(fake.pathCalls("chat.postMessage")), "I was restarted while")
	restart := fake.callCount()

	row, ok, err := shared.ThreadRecord(t.Context(), "slack", "D1", "555.000")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.Delivered{TextLen: len(opening), ToolSteps: 2}, row.Delivered,
		"the row records what the first process delivered")

	turn := channels.InFlightTurn{
		Msg:       channels.InboundMessage{Channel: "slack", ChannelID: "D1", ThreadID: "555.000", AgentRef: row.AgentRef, Resume: row.Resume},
		TaskID:    row.TaskID,
		Delivered: row.Delivered,
	}
	gw2 := &stubGateway{records: shared, resumes: &stubResumes{
		durable: true,
		turns:   []channels.InFlightTurn{turn},
		deltas:  map[string][]channels.OutboundDelta{"task-9": {toolStep("team"), {Content: opening + tail}, {Done: true}}},
	}}
	a2, _ := newEventsAdapter(t, gw2, api.URL)
	a2.RecoverTurns()

	require.Eventually(t, func() bool {
		return strings.Contains(textOf(fake.callsSince(restart)), tail)
	}, flowWait, 20*time.Millisecond, "the tail of the answer is posted after the restart")
	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(fake.reactionNames("reactions.add"), " "), "white_check_mark")
	}, flowWait, 20*time.Millisecond, "the continued turn completes")

	after := textOf(fake.callsSince(restart))
	require.NotContains(t, after, "One thing to flag", "the opening the first process posted is not posted again")
	require.Equal(t, 1, callsCarrying(fake.pathCalls(pathStartStream), "One thing to flag"), "the thread carries the opening once")
	var ids []string
	for _, s := range fake.streamedSteps() {
		ids = append(ids, s["id"].(string)+" "+s["status"].(string))
	}
	require.Equal(t, []string{
		"step-1 in_progress", "step-2 in_progress",
		// The shutdown closes what the first process had running; the
		// continuation numbers on and its own end closes step-3.
		"step-1 error", "step-2 error",
		"step-3 in_progress", "step-3 complete",
	}, ids, "the step ids count on from the recorded ones")
	streams := fake.pathCalls(pathStartStream)
	require.Len(t, streams, 2, "the continuation opens a message of its own; the first one was closed on shutdown")
	require.Equal(t, "555.000", streams[1].params["thread_ts"], "the continuation lands in the thread")

	row, _, err = shared.ThreadRecord(t.Context(), "slack", "D1", "555.000")
	require.NoError(t, err)
	require.Equal(t, len(opening+tail), row.Delivered.TextLen, "the record now covers the whole answer")
	require.Equal(t, 3, row.Delivered.ToolSteps)
}

// The whole answer had landed before the restart: the continued turn has
// nothing to add, and says so instead of repeating the reply or reporting no
// reply at all.
func TestRecoverTurns_NothingLeftToPostSaysSo(t *testing.T) {
	fake := newFakeSlackAPI()
	const answer = "All three clusters run the same chart version."
	turn := leftoverTurn("task-9")
	turn.Delivered = store.Delivered{TextLen: len(answer)}
	gw := &stubGateway{resumes: &stubResumes{
		durable: true,
		turns:   []channels.InFlightTurn{turn},
		deltas:  map[string][]channels.OutboundDelta{"task-9": {{Content: answer}, {Done: true}}},
	}}
	a, _ := newEventsAdapter(t, gw, fake.server(t).URL)

	a.RecoverTurns()

	require.Eventually(t, func() bool {
		return strings.Contains(allBlockText(fake.pathCalls("chat.postMessage")), "the reply above is complete")
	}, flowWait, 20*time.Millisecond, "the thread is told the reply was complete")
	posted := textOf(fake.callsSince(0))
	require.NotContains(t, posted, "same chart version", "the answer is not posted a second time")
	require.NotContains(t, posted, "finished without a reply", "a delivered answer is not reported as none")
}
