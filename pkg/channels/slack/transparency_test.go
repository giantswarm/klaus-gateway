package slack

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestRecordTurnUsage_LastAndSession(t *testing.T) {
	a := &Adapter{}
	require.Equal(t, "Token usage not available yet.", a.usageReport(t.Context(), "T1", "D1"))

	a.recordTurnUsage("T1", "C1", channels.TurnUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150})
	a.recordTurnUsage("T1", "C1", channels.TurnUsage{InputTokens: 30, OutputTokens: 20, TotalTokens: 50})

	report := a.usageReport(t.Context(), "T1", "C1")
	require.Contains(t, report, "Last turn — in 30 · out 20 · total 50")
	require.Contains(t, report, "Session — in 130 · out 70 · total 200")

	// An empty turn must not clobber the last-turn figures.
	a.recordTurnUsage("T1", "C1", channels.TurnUsage{})
	require.Contains(t, a.usageReport(t.Context(), "T1", "C1"), "Last turn — in 30 · out 20 · total 50")
}

// A top-level /usage in a DM keys a brand-new thread (its own ts); the report
// must fall back to the DM channel's aggregated usage instead of claiming no
// usage exists.
func TestUsageReport_DMTopLevelFallsBackToChannel(t *testing.T) {
	a := &Adapter{}
	a.recordTurnUsage("100.000", "D1", channels.TurnUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15})
	a.recordTurnUsage("200.000", "D1", channels.TurnUsage{InputTokens: 30, OutputTokens: 20, TotalTokens: 50})

	// "300.000" is the /usage message's own ts: no turn ever ran in that thread.
	report := a.usageReport(t.Context(), "300.000", "D1")
	require.Contains(t, report, "Last turn — in 30 · out 20 · total 50")
	require.Contains(t, report, "Session — in 40 · out 25 · total 65",
		"the DM fallback reports the channel aggregate across threads")

	// An in-thread /usage in the DM still reports that thread's own figures.
	inThread := a.usageReport(t.Context(), "100.000", "D1")
	require.Contains(t, inThread, "Session — in 10 · out 5 · total 15")
}

// In a regular channel a missed lookup means the command was typed outside the
// agent's thread; the reply guides the user there instead of the misleading
// "not available yet".
func TestUsageReport_ChannelMissGivesGuidance(t *testing.T) {
	a := &Adapter{}
	a.recordTurnUsage("100.000", "C1", channels.TurnUsage{TotalTokens: 5})

	report := a.usageReport(t.Context(), "999.000", "C1")
	require.Contains(t, report, "as a reply inside the agent's thread")
	require.NotContains(t, report, "not available yet")

	// Channel turns must not leak into a DM-style channel aggregate.
	a.usageMu.Lock()
	require.NotContains(t, a.channelUsage, "C1")
	a.usageMu.Unlock()
}

// TestBatchedWriter_SumsUsageAcrossTurn verifies the run loop sums the per-call
// usage kagent reports into a single turn total.
func TestBatchedWriter_SumsUsageAcrossTurn(t *testing.T) {
	w := newBatchedWriterWithClient(&slackAPIClient{}, "C1", "", "T1", nil)

	ch := make(chan channels.OutboundDelta, 3)
	ch <- channels.OutboundDelta{Usage: &channels.TurnUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}}
	ch <- channels.OutboundDelta{Usage: &channels.TurnUsage{InputTokens: 30, OutputTokens: 20, TotalTokens: 50}}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))
	require.Equal(t, channels.TurnUsage{InputTokens: 130, OutputTokens: 70, TotalTokens: 200}, w.turnUsage)
}

// TestBatchedWriter_ToolStepsPreserveOrder verifies the steps reach Slack in
// stream order, inside the one message the turn streams.
func TestBatchedWriter_ToolStepsPreserveOrder(t *testing.T) {
	ft := &fakeThread{}
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "", "T1", nil)

	names := []string{"alpha", "bravo", "charlie", "delta"}
	ch := make(chan channels.OutboundDelta, len(names)+1)
	for _, n := range names {
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: n, Kind: channels.ToolCall}}
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))

	steps := ft.steps()
	require.Len(t, steps, 2*len(names), "each call opens a step; none got a result, so the turn's end closes them")
	for i, n := range names {
		require.Equal(t, fmt.Sprintf("step-%d", i+1), steps[i].id)
		require.Equal(t, stepInProgress, steps[i].status)
		require.Contains(t, steps[i].details, n, "the steps stay in stream order")
		require.Equal(t, steps[i].id, steps[len(names)+i].id, "and are closed in the same order")
		require.Equal(t, stepComplete, steps[len(names)+i].status)
	}
	require.Len(t, ft.finalMessages(), 1, "a tool-heavy turn is still one message")
}

// A tool storm costs the thread exactly ONE message — the reply, with the steps
// inside it — however many calls stream, and no post of its own.
func TestBatchedWriter_ToolStormIsOneMessage(t *testing.T) {
	ft := &fakeThread{}
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "", "T1", nil)

	const calls = 50
	ch := make(chan channels.OutboundDelta, calls+1)
	for range calls {
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: "list_pods", Kind: channels.ToolCall}}
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))

	require.Len(t, ft.finalMessages(), 1)
	require.Len(t, ft.steps(), 2*calls, "one update per call, plus the close the turn's end sends")
	require.Equal(t, 0, ft.postCount(), "the steps never cost a message of their own")
}

// A turn past the step cap opens no further steps and says so once. The calls
// are still recorded for the "Inspect agent steps" shortcut.
func TestBatchedWriter_CapsSteps(t *testing.T) {
	ft := &fakeThread{}
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	a := &Adapter{Logger: testLogger()}
	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "", "T1", nil)
	w.adapter = a

	const calls = maxSteps + 5
	ch := make(chan channels.OutboundDelta, 2*calls+1)
	for i := range calls {
		id := fmt.Sprintf("c%d", i)
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Name: "list_pods", Kind: channels.ToolCall, CallID: id,
		}}
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{
			Name: "list_pods", Kind: channels.ToolResult, CallID: id, Response: map[string]any{"output": "ok"},
		}}
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))

	steps := ft.steps()
	require.Len(t, steps, 2*maxSteps, "each of the capped calls opens and closes its step; the rest open none")
	require.Equal(t, "step-"+strconv.Itoa(maxSteps), steps[len(steps)-1].id)
	require.Equal(t, 1, strings.Count(ft.streamedText(), stepLimitNote), "the note is sent once")

	entries, dropped := a.toolLogSnapshot("T1")
	require.Equal(t, 2*calls, len(entries)+dropped,
		"every call is still recorded for the inspection shortcut, under its own cap")
}

func TestCompactJSON_TruncatesAndEmpty(t *testing.T) {
	require.Equal(t, "", compactJSON(nil, 100))
	require.Equal(t, "", compactJSON(map[string]any{}, 100))
	require.Equal(t, `{"a": "b"}`, compactJSON(map[string]any{"a": "b"}, 100))

	out := compactJSON(map[string]any{"k": "0123456789"}, 8)
	require.Len(t, []rune(out), 9, "8 runes + ellipsis")
	require.Contains(t, out, "…")
}

// fakeModelSource counts lookups and returns a fixed model.
type fakeModelSource struct {
	calls    atomic.Int32
	model    string
	provider string
}

func (f *fakeModelSource) AgentModel(_ context.Context, _ string) (string, string, error) {
	f.calls.Add(1)
	return f.model, f.provider, nil
}

func TestUsageReport_IncludesModelLineAndCaches(t *testing.T) {
	source := &fakeModelSource{model: "gpt-5", provider: "OpenAI"}
	a := &Adapter{DefaultAgent: "kagent/sre-agent", Models: source}
	a.recordTurnUsage("T1", "C1", channels.TurnUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3})

	report := a.usageReport(t.Context(), "T1", "C1")
	require.Contains(t, report, "Model — OpenAI/gpt-5")

	_ = a.usageReport(t.Context(), "T1", "C1")
	require.Equal(t, int32(1), source.calls.Load(), "model lookups must be cached")
}

// perRefModelSource returns a distinct model per agentRef, so a test can tell
// which agent's model a report resolved.
type perRefModelSource struct{}

func (perRefModelSource) AgentModel(_ context.Context, agentRef string) (string, string, error) {
	return "model-of-" + agentRef, "", nil
}

// In a thread bound to a non-default agent (/agent selection), the /usage
// model line names the bound agent's model, not the default's.
func TestUsageReport_ModelLineFollowsThreadBinding(t *testing.T) {
	a := &Adapter{DefaultAgent: "kagent/default-agent", Models: perRefModelSource{}}
	a.bindThreadAgent(t.Context(), "C1", "T1", "kagent/sre-agent")
	a.recordTurnUsage("T1", "C1", channels.TurnUsage{TotalTokens: 3})

	require.Contains(t, a.usageReport(t.Context(), "T1", "C1"), "Model — model-of-kagent/sre-agent")

	// An unbound thread still reports the default agent's model.
	a.recordTurnUsage("T2", "C1", channels.TurnUsage{TotalTokens: 3})
	require.Contains(t, a.usageReport(t.Context(), "T2", "C1"), "Model — model-of-kagent/default-agent")
}

// A BYO agent exposes no model; the line is omitted rather than rendered empty.
func TestUsageReport_OmitsModelLineWhenUnavailable(t *testing.T) {
	a := &Adapter{DefaultAgent: "kagent/sre-agent", Models: &fakeModelSource{}}
	a.recordTurnUsage("T1", "C1", channels.TurnUsage{TotalTokens: 3})
	require.NotContains(t, a.usageReport(t.Context(), "T1", "C1"), "Model")

	noSource := &Adapter{DefaultAgent: "kagent/sre-agent"}
	noSource.recordTurnUsage("T1", "C1", channels.TurnUsage{TotalTokens: 3})
	require.NotContains(t, noSource.usageReport(t.Context(), "T1", "C1"), "Model")
}

// Idle per-thread state (usage, resume marks) is swept on insert once past
// threadStateTTL, so a long-lived pod does not accumulate one entry per thread
// forever. synctest fakes time.Now inside the bubble.
func TestThreadState_EvictedAfterTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &Adapter{Logger: slog.New(slog.DiscardHandler)}
		a.recordTurnUsage("T-old", "D-old", channels.TurnUsage{TotalTokens: 1})
		a.resumeMu.Lock()
		a.resumeChecked = map[string]time.Time{"T-old": time.Now().Add(threadStateTTL)}
		a.resumeMu.Unlock()

		time.Sleep(threadStateTTL + time.Minute)

		// Inserts sweep the expired entries.
		a.recordTurnUsage("T-new", "D-new", channels.TurnUsage{TotalTokens: 2})
		gw := &resumeStub{exists: true, checked: true}
		a.gw = gw
		a.maybeAnnounceResume(t.Context(), channels.InboundMessage{ThreadID: "T-new"}, "D-new")

		a.usageMu.Lock()
		require.NotContains(t, a.threadUsage, "T-old")
		require.NotContains(t, a.channelUsage, "D-old")
		require.Contains(t, a.threadUsage, "T-new")
		a.usageMu.Unlock()

		a.resumeMu.Lock()
		require.NotContains(t, a.resumeChecked, "T-old")
		require.Contains(t, a.resumeChecked, "T-new")
		a.resumeMu.Unlock()
	})
}

// resumeStub is a minimal Gateway with a canned SessionResumable answer.
type resumeStub struct {
	exists, checked bool
	calls           atomic.Int32
}

func (r *resumeStub) Resolve(context.Context, channels.InboundMessage) (channels.InstanceRef, error) {
	return channels.InstanceRef{}, nil
}

func (r *resumeStub) SendCompletion(context.Context, channels.InstanceRef, channels.InboundMessage) (<-chan channels.OutboundDelta, error) {
	return nil, nil
}

func (r *resumeStub) FetchHistory(context.Context, channels.InstanceRef) ([]channels.Message, error) {
	return nil, nil
}

func (r *resumeStub) SessionResumable(context.Context, channels.InboundMessage) (bool, bool) {
	r.calls.Add(1)
	return r.exists, r.checked
}

// A transient failure of the resume check must not permanently suppress the
// notice: only a conclusive result marks the thread as checked.
func TestMaybeAnnounceResume_RetriesAfterTransientError(t *testing.T) {
	srv := &fakeSlackServer{}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	a := &Adapter{APIBase: ts.URL, Secrets: Secrets{BotToken: "test-bot-token"}, Logger: slog.New(slog.DiscardHandler)} //nolint:gosec
	gw := &resumeStub{exists: false, checked: false}
	a.gw = gw
	msg := channels.InboundMessage{ThreadID: "100.000"}

	// Transient error: no notice, thread stays unmarked.
	a.maybeAnnounceResume(t.Context(), msg, "D1")
	require.Equal(t, int32(1), gw.calls.Load())
	require.Equal(t, int32(0), srv.posts.Load())

	// The next message retries and gets the conclusive "gone" answer.
	gw.checked = true
	a.maybeAnnounceResume(t.Context(), msg, "D1")
	require.Equal(t, int32(2), gw.calls.Load())
	require.Equal(t, int32(1), srv.posts.Load(), "the starting-fresh notice is posted")

	// Conclusive results are remembered: no further checks or notices.
	a.maybeAnnounceResume(t.Context(), msg, "D1")
	require.Equal(t, int32(2), gw.calls.Load())
	require.Equal(t, int32(1), srv.posts.Load())
}
