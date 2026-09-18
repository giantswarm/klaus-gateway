package slack

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// runContinued drives a writer seeded with what a previous process delivered
// over deltas (plus a closing Done) in reactions mode, and returns the thread
// as a user sees it and every delivery record the writer reported.
func runContinued(t *testing.T, carried store.Delivered, deltas ...channels.OutboundDelta) ([]capturedMessage, []store.Delivered) {
	t.Helper()
	ft := &fakeThread{}
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	client := &slackAPIClient{botToken: "t", baseURL: srv.URL}
	w := newBatchedWriterWithClient(client, "C1", "", "1.0", detailsOn, slog.Default())
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
	return ft.finalMessages(), records
}

func textDelta(text string) channels.OutboundDelta {
	return channels.OutboundDelta{Kind: channels.DeltaText, Content: text}
}

// threadText flattens the thread into one string for containment checks.
func threadText(msgs []capturedMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(strings.Join(m, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// A writer continuing a turn drops the answer text the previous process
// posted — the whole answer arrives at completion — and the receipt counts
// the steps on from the recorded segment, names included (klaus-gateway#301).
func TestContinueFrom_PostsOnlyWhatFollowsAndCountsOn(t *testing.T) {
	const opening = "Created. One thing to flag: the repository is private."
	const tail = "Next: rotate the deploy token."
	carried := store.Delivered{TextLen: len(opening), ToolSteps: 2, ToolOrder: []string{"create", "protect"}, ToolCounts: map[string]int{"create": 1, "protect": 1}}

	msgs, records := runContinued(t, carried, toolCallDelta("team"), textDelta(opening+"\n\n"+tail))

	require.Len(t, msgs, 2, "the receipt and the continued answer: %v", msgs)
	require.Equal(t, capturedMessage{"🛠️ 3 steps · create · protect · team"}, msgs[0], "the receipt continues the recorded segment")
	require.Equal(t, capturedMessage{tail}, msgs[1], "only the text after the recorded length is posted, without the paragraph break in front")
	require.NotContains(t, threadText(msgs), "One thing to flag")

	last := records[len(records)-1]
	require.Equal(t, len(opening+"\n\n"+tail), last.TextLen, "the record covers the whole answer, the dropped break included")
	require.Equal(t, 3, last.ToolSteps)
	require.Equal(t, []string{"create", "protect", "team"}, last.ToolOrder)
}

// The previous process posted the receipt of the segment it was seeded from
// when it shut down; a narration that closes that segment before any new
// step posts no second copy, and the steps after the narration start a fresh
// count as they would in one process.
func TestContinueFrom_SegmentClosedWithoutANewStepPostsNoSecondReceipt(t *testing.T) {
	carried := store.Delivered{TextLen: 5, ToolSteps: 2, ToolOrder: []string{"get"}, ToolCounts: map[string]int{"get": 2}}

	msgs, records := runContinued(t, carried,
		narrationDelta("Both share the same chart version."),
		toolCallDelta("diff"),
		textDelta("hello world"),
	)

	text := threadText(msgs)
	require.NotContains(t, text, "2 steps", "the seeded segment's receipt is not posted again")
	require.Contains(t, text, "🛠️ 1 step · diff", "the segment after the narration counts from one")
	require.Contains(t, text, "world", "the text after the recorded length lands")
	require.NotContains(t, text, "hello world", "the first five bytes are the previous process's")

	closed := slices.IndexFunc(records, func(d store.Delivered) bool { return d.ToolSteps == 0 })
	require.GreaterOrEqual(t, closed, 0, "the narration records the closed segment as no open steps")
	require.Equal(t, 5, records[closed].TextLen, "the recorded text length is kept across the segment close")
}

// A writer that continues nothing works as before: no text is dropped, the
// count starts at one, and every step and flush is recorded for a restart.
func TestNoteDelivered_RecordsTextAndTheOpenSegment(t *testing.T) {
	msgs, records := runContinued(t, store.Delivered{},
		toolCallDelta("get"), toolCallDelta("get"),
		narrationDelta("Looking at the second cluster now."),
		toolCallDelta("list"),
		textDelta("hello"),
	)

	require.Contains(t, threadText(msgs), "hello")
	require.Equal(t, store.Delivered{TextLen: 5, ToolSteps: 1, ToolOrder: []string{"list"}, ToolCounts: map[string]int{"list": 1}}, records[len(records)-1],
		"the last record is the flushed text and the segment open at the end")
	require.True(t, slices.ContainsFunc(records, func(d store.Delivered) bool {
		return d.ToolSteps == 2 && d.ToolCounts["get"] == 2 && d.TextLen == 0
	}), "the first segment's steps were recorded as they happened: %v", records)
	require.True(t, slices.ContainsFunc(records, func(d store.Delivered) bool { return d.ToolSteps == 0 }),
		"the narration recorded the segment as closed: %v", records)
}

// Without a sink nothing is recorded and the writer behaves as before.
func TestNoteDelivered_WithoutASinkIsANoOp(t *testing.T) {
	w := newBatchedWriterWithClient(&slackAPIClient{}, "C1", "", "1.0", detailsOn, slog.Default())
	w.noteDelivered(t.Context())
	require.Equal(t, "hello", w.skipDelivered("hello"), "no continuation, no cut")
}

// A cut inside a delta leaves the rest of that delta; the skip spans deltas
// until the recorded length is reached; leading whitespace after the cut is
// dropped only once, and counted.
func TestSkipDelivered_SpansDeltasAndTrimsTheBreakOnce(t *testing.T) {
	w := newBatchedWriterWithClient(&slackAPIClient{}, "C1", "", "1.0", detailsOn, slog.Default())
	w.continueFrom(store.Delivered{TextLen: 7})

	require.Equal(t, "", w.skipDelivered("hello"))
	require.Equal(t, "", w.skipDelivered(" w"), "the cut lands at the end of this delta")
	require.Equal(t, "", w.skipDelivered("\n\n"), "a whitespace-only delta after the cut goes")
	require.Equal(t, "orld", w.skipDelivered("  orld"), "the break in front of the continued text goes with it")
	require.Equal(t, " next", w.skipDelivered(" next"), "later deltas are untouched")
	require.Equal(t, 4, w.leadTrimmed, "the dropped whitespace counts toward the delivered length")
}
