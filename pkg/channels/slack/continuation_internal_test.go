package slack

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// runContinuedOn drives a writer seeded with what a previous process delivered
// over deltas (plus a closing Done) in reactions mode, against a thread the
// caller set up, so a test can seed a stream for the writer to adopt and then
// read the calls it made.
func runContinuedOn(t *testing.T, ft *fakeThread, carried store.Delivered, deltas ...channels.OutboundDelta) ([]capturedMessage, []store.Delivered, *batchedWriter) {
	t.Helper()
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", "", "1.0", slog.Default())
	w.adapter = &Adapter{}
	w.continueFrom(carried)
	var mu sync.Mutex
	var records []store.Delivered
	w.onDelivered = func(_ context.Context, d store.Delivered) {
		mu.Lock()
		defer mu.Unlock()
		records = append(records, d)
	}
	ch := make(chan channels.OutboundDelta, len(deltas)+1)
	for _, d := range deltas {
		ch <- d
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)
	require.NoError(t, w.run(t.Context(), ch))

	mu.Lock()
	defer mu.Unlock()
	return ft.finalMessages(), records, w
}

func textDelta(text string) channels.OutboundDelta {
	return channels.OutboundDelta{Kind: channels.DeltaText, Content: text}
}

// A writer continuing a turn drops the answer text the previous process
// posted — the whole answer arrives at completion — and numbers its steps on
// from the recorded count, so no id already on the reply is reused
// (klaus-gateway#301).
func TestContinueFrom_PostsOnlyWhatFollowsAndCountsOn(t *testing.T) {
	const opening = "Created. One thing to flag: the repository is private."
	const tail = "Next: rotate the deploy token."
	carried := store.Delivered{TextLen: len(opening), ToolSteps: 2}

	ft := &fakeThread{}
	_, records, _ := runContinuedOn(t, ft, carried, toolCallDelta("team"), textDelta(opening+"\n\n"+tail))

	require.Equal(t, []taskChunk{
		{id: "step-3", title: "Team", status: stepInProgress},
		{id: "step-3", title: "Team", status: stepComplete},
	}, ft.stepShapes(), "the step counts on from the recorded ones; the record named none as open")
	require.Equal(t, tail, ft.streamedText(),
		"only the text after the recorded length is sent, without the paragraph break in front")

	last := records[len(records)-1]
	require.Equal(t, len(opening+"\n\n"+tail), last.TextLen, "the record covers the whole answer, the dropped break included")
	require.Equal(t, 3, last.ToolSteps)
}

// The previous process died with a step still running on the message this one
// adopts, and the record names it. That step is closed under its own title
// before anything else goes out — Slack would keep it spinning otherwise — and
// the turn's next call opens the step after it.
func TestContinueFrom_ClosesTheCarriedStepThenCountsOn(t *testing.T) {
	ft := &fakeThread{}
	ts := openStreamOn(t, ft, "Looking. ")
	carried := store.Delivered{
		TextLen: 9, StreamTS: ts, StreamLen: 9, ToolSteps: 3,
		OpenStepID: "step-3", OpenStepTitle: "Prometheus query",
	}

	_, records, _ := runContinuedOn(t, ft, carried, toolCallDelta("x_kubernetes_list"), textDelta("Looking. Done."))

	require.Equal(t, []taskChunk{
		{id: "step-3", title: "Prometheus query", status: stepComplete},
		{id: "step-4", title: "Kubernetes list", status: stepInProgress},
		{id: "step-4", title: "Kubernetes list", status: stepComplete},
	}, ft.stepShapes(), "the carried step is closed under its real title, the new one counts on")
	require.Equal(t, ts, ft.streams()[1].ts, "both ride the adopted stream")

	last := records[len(records)-1]
	require.Equal(t, 4, last.ToolSteps)
	require.Empty(t, last.OpenStepID, "nothing is left running")
}

// A restart that fell between a step's result and the answer's last words
// leaves no step running, so the continuation touches none of them — a finished
// step keeps its real title and an error step stays an error.
func TestContinueFrom_NoOpenStepTouchesNoStep(t *testing.T) {
	ft := &fakeThread{}
	ts := openStreamOn(t, ft, "Looking. ")
	carried := store.Delivered{TextLen: 9, StreamTS: ts, StreamLen: 9, ToolSteps: 3}

	msgs, _, _ := runContinuedOn(t, ft, carried, textDelta("Looking. Done."))

	require.Empty(t, ft.steps(), "the record named no open step, so none is rewritten")
	require.Equal(t, []capturedMessage{{"Looking. Done."}}, msgs, "only the text after the recorded length is added")
}

// The whole answer had landed before the restart, so the continuation adds
// nothing — but the step the previous process was running still has to be
// closed on the adopted message before it is stopped.
func TestContinueFrom_NothingToAddStillClosesTheOpenStep(t *testing.T) {
	const answer = "All three clusters run the same chart version."
	ft := &fakeThread{}
	ts := openStreamOn(t, ft, answer)
	carried := store.Delivered{
		TextLen: len(answer), StreamTS: ts, StreamLen: len(answer), ToolSteps: 2,
		OpenStepID: "step-2", OpenStepTitle: "Kubernetes get",
	}

	_, _, w := runContinuedOn(t, ft, carried, textDelta(answer))

	require.Equal(t, []taskChunk{{id: "step-2", title: "Kubernetes get", status: stepComplete}}, ft.steps())
	require.Equal(t, []string{methodChatStartStream, methodChatAppendStream, methodChatStopStream}, ft.streamMethods(),
		"the close rides an append on the adopted message, which is then stopped")
	require.False(t, w.wroteContent(), "closing a step is not a reply of this process's own")
}

// A writer that continues nothing works as before: no text is dropped, the
// step count starts at one, and every step and flush is recorded for a restart.
func TestNoteDelivered_RecordsTextAndTheStepCount(t *testing.T) {
	ft := &fakeThread{}
	_, records, _ := runContinuedOn(t, ft, store.Delivered{},
		toolCallDelta("get"), toolCallDelta("get"),
		narrationDelta("Looking at the second cluster now."),
		toolCallDelta("list"),
		textDelta("hello"),
	)

	require.Contains(t, ft.streamedText(), "hello")
	require.Equal(t, store.Delivered{TextLen: 5, ToolSteps: 3}, records[len(records)-1],
		"the last record is the answer text and every step id issued, the stream closed")
	require.True(t, slices.ContainsFunc(records, func(d store.Delivered) bool {
		return d.ToolSteps == 2 && d.TextLen == 0
	}), "the steps were recorded as they happened: %v", records)
}

// Without a sink nothing is recorded and the writer behaves as before.
func TestNoteDelivered_WithoutASinkIsANoOp(t *testing.T) {
	w := newBatchedWriterWithClient(&slackAPIClient{}, "C1", "", "1.0", slog.Default())
	w.noteDelivered(t.Context())
	require.Equal(t, "hello", w.skipDelivered("hello"), "no continuation, no cut")
}

// A cut inside a delta leaves the rest of that delta; the skip spans deltas
// until the recorded length is reached; leading whitespace after the cut is
// dropped only once, and counted.
func TestSkipDelivered_SpansDeltasAndTrimsTheBreakOnce(t *testing.T) {
	w := newBatchedWriterWithClient(&slackAPIClient{}, "C1", "", "1.0", slog.Default())
	w.continueFrom(store.Delivered{TextLen: 7})

	require.Equal(t, "", w.skipDelivered("hello"))
	require.Equal(t, "", w.skipDelivered(" w"), "the cut lands at the end of this delta")
	require.Equal(t, "", w.skipDelivered("\n\n"), "a whitespace-only delta after the cut goes")
	require.Equal(t, "orld", w.skipDelivered("  orld"), "the break in front of the continued text goes with it")
	require.Equal(t, " next", w.skipDelivered(" next"), "later deltas are untouched")
	require.Equal(t, 4, w.leadTrimmed, "the dropped whitespace counts toward the delivered length")
}

// openStreamOn seeds ft with a stream a previous process left open, the way a
// gateway killed mid-answer leaves one, and returns its ts.
func openStreamOn(t *testing.T, ft *fakeThread, text string) string {
	t.Helper()
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)
	c := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	ts, err := c.startStream(t.Context(), "C1", "1.0", []any{textChunk(text)}, "", "")
	require.NoError(t, err)
	return ts
}

// A turn continued after a restart appends the rest of the answer to the
// stream the previous process left open, so the reply reads as one message,
// and closes that message with the turn's exit status.
func TestContinueFrom_AppendsToTheAdoptedStream(t *testing.T) {
	const opening = "Created. "
	const tail = "Next: rotate the deploy token."
	ft := &fakeThread{}
	ts := openStreamOn(t, ft, opening)

	msgs, records, _ := runContinuedOn(t, ft,
		store.Delivered{TextLen: len(opening), StreamTS: ts, StreamLen: len(opening)},
		textDelta(opening+tail))

	require.Equal(t, []string{methodChatStartStream, methodChatAppendStream, methodChatStopStream}, ft.streamMethods())
	require.Equal(t, ts, ft.streams()[1].ts, "the continuation writes to the message already open")
	require.Equal(t, []string{string(sessionActive)}, ft.stopStatuses(), "the adopted stream carries the exit status")
	require.Equal(t, []capturedMessage{{opening + tail}}, msgs, "the reply reads as one message")
	require.Equal(t, []string{"processing", "active"}, ft.statuses(), "and the session ends with the turn")

	last := records[len(records)-1]
	require.Equal(t, len(opening+tail), last.TextLen)
	require.Empty(t, last.StreamTS, "the closed stream is no longer offered for adoption")
}

// Slack closed the adopted message while the gateway was down: the append is
// refused, and the rest of the answer opens a stream of its own — the turn's
// one recovery.
func TestContinueFrom_AdoptedStreamGoneFallsBackToANewStream(t *testing.T) {
	const opening = "Created. "
	const tail = "Next: rotate the deploy token."
	ft := &fakeThread{}
	ts := openStreamOn(t, ft, opening)
	ft.haltStream(ts)

	msgs, _, w := runContinuedOn(t, ft,
		store.Delivered{TextLen: len(opening), StreamTS: ts, StreamLen: len(opening)},
		textDelta(opening+tail))

	require.Equal(t, []string{methodChatStartStream, methodChatAppendStream, methodChatStartStream, methodChatStopStream},
		ft.streamMethods())
	require.True(t, w.streamRecovered)
	require.Equal(t, []string{string(sessionActive)}, ft.stopStatuses())
	require.Equal(t, []capturedMessage{{opening}, {tail}}, msgs, "the rest lands in a message of its own")
}

// The whole answer had landed before the restart: there is nothing left to
// append, and the message the previous process left open is still closed, so
// it stops animating and the indicator clears with it.
func TestContinueFrom_NothingLeftStillClosesTheAdoptedStream(t *testing.T) {
	const answer = "All three clusters run the same chart version."
	ft := &fakeThread{}
	ts := openStreamOn(t, ft, answer)

	msgs, _, w := runContinuedOn(t, ft,
		store.Delivered{TextLen: len(answer), StreamTS: ts, StreamLen: len(answer)},
		textDelta(answer))

	require.Equal(t, []string{methodChatStartStream, methodChatStopStream}, ft.streamMethods())
	require.Equal(t, []string{string(sessionActive)}, ft.stopStatuses())
	require.Equal(t, []capturedMessage{{answer}}, msgs, "the answer is not repeated")
	require.False(t, w.wroteContent(), "nothing new was appended, so the turn says the reply was complete")
}

// The record advances with every append: a restart between two of them
// continues on the same message, from the byte the last one delivered.
func TestNoteDelivered_RecordsTheOpenStreamAsItGrows(t *testing.T) {
	ft := &fakeThread{}
	w := streamWriter(t, ft, "D1")
	var records []store.Delivered
	w.onDelivered = func(_ context.Context, d store.Delivered) { records = append(records, d) }

	w.queueAnswer("first part ")
	require.NoError(t, w.flush(t.Context()))
	w.queueAnswer("second part ")
	require.NoError(t, w.flush(t.Context()))

	require.Len(t, records, 2)
	require.Equal(t, store.Delivered{TextLen: 11, StreamTS: "msg-1", StreamLen: 11}, records[0])
	require.Equal(t, store.Delivered{TextLen: 23, StreamTS: "msg-1", StreamLen: 23}, records[1],
		"the second append advances the text and the open message together")

	require.NoError(t, w.closeStream(t.Context()))
	require.Equal(t, store.Delivered{TextLen: 23}, records[len(records)-1],
		"the closing stop leaves no stream to adopt")
}

// A continued turn that ends as a connector sign-in prompt retracts the message
// it adopted too: the prompt replaces the whole reply, not just the part this
// process wrote.
func TestContinueFrom_RetractDeletesTheAdoptedStream(t *testing.T) {
	ft := &fakeThread{}
	ts := openStreamOn(t, ft, "Sign in at ")
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{botToken: "t", baseURL: srv.URL}, "C1", "", "1.0", slog.Default())
	w.continueFrom(store.Delivered{TextLen: 11, StreamTS: ts, StreamLen: 11})

	w.retractRendered(t.Context())

	require.Equal(t, []string{string(sessionProcessing)}, ft.stopStatuses(), "a streaming message is stopped before it is deleted")
	require.Equal(t, []string{ts}, ft.deleted())
	require.Empty(t, ft.finalMessages(), "nothing of the reply is left in the thread")
}
