package slack

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestDetailsLevel_DefaultOnAndSet(t *testing.T) {
	a := &Adapter{}
	require.Equal(t, detailsOn, a.detailsLevel("T1"), "an un-set thread defaults to on")

	a.setDetailsLevel("T1", detailsOff)
	require.Equal(t, detailsOff, a.detailsLevel("T1"))
	require.Equal(t, "off", a.detailsLevel("T1").String())

	a.setDetailsLevel("T1", detailsFull)
	require.Equal(t, detailsFull, a.detailsLevel("T1"))
	require.Equal(t, detailsOn, a.detailsLevel("T2"), "other threads unaffected")
}

func TestParseDetailsLevel(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  detailsLevel
		wantK bool
	}{
		{"on", detailsOn, true},
		{"off", detailsOff, true},
		{"FULL", detailsFull, true},
		{"maybe", detailsOn, false},
	} {
		got, ok := parseDetailsLevel(tc.in)
		require.Equal(t, tc.wantK, ok, tc.in)
		if tc.wantK {
			require.Equal(t, tc.want, got, tc.in)
		}
	}
}

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
	w := newBatchedWriterWithClient(&slackAPIClient{}, "C1", "", "T1", detailsOn, nil)

	ch := make(chan channels.OutboundDelta, 3)
	ch <- channels.OutboundDelta{Usage: &channels.TurnUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}}
	ch <- channels.OutboundDelta{Usage: &channels.TurnUsage{InputTokens: 30, OutputTokens: 20, TotalTokens: 50}}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))
	require.Equal(t, channels.TurnUsage{InputTokens: 130, OutputTokens: 70, TotalTokens: 200}, w.turnUsage)
}

// TestBatchedWriter_CapsToolActivity verifies a tool-heavy detailsFull turn
// stays bounded: at most maxToolEntries entries plus one truncation note,
// aggregated into activity messages that respect the per-message block budget
// instead of one post per call.
func TestBatchedWriter_CapsToolActivity(t *testing.T) {
	ft := &fakeThread{}
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "", "T1", detailsFull, nil)

	ch := make(chan channels.OutboundDelta, maxToolEntries+6)
	for range maxToolEntries + 5 {
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: "list_pods", Kind: channels.ToolCall}}
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))

	msgs := ft.finalMessages()
	var entries []string
	for _, m := range msgs {
		require.LessOrEqual(t, len(m), maxActivityBlocks, "one activity message stays within the block budget")
		entries = append(entries, m...)
	}
	require.Len(t, entries, maxToolEntries+1, "capped entries plus the truncation note")
	require.Equal(t, toolLimitNote, entries[maxToolEntries])
	require.Len(t, msgs, 2, "entries aggregate into activity messages, rolling over past the block budget")
}

// TestBatchedWriter_ToolPostsPreserveOrder verifies the async poster keeps
// detailsFull tool entries in stream order, aggregated into one activity
// message, and delivers them all before run() returns.
func TestBatchedWriter_ToolPostsPreserveOrder(t *testing.T) {
	ft := &fakeThread{}
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "", "T1", detailsFull, nil)

	names := []string{"alpha", "bravo", "charlie", "delta"}
	ch := make(chan channels.OutboundDelta, len(names)+1)
	for _, n := range names {
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: n, Kind: channels.ToolCall}}
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))

	// run() drained the poster before returning: every entry is already delivered.
	msgs := ft.finalMessages()
	require.Len(t, msgs, 1, "consecutive tool calls share one activity message")
	require.Len(t, msgs[0], len(names))
	for i, n := range names {
		require.Contains(t, msgs[0][i], "`"+n+"`", "tool entries must stay in stream order")
	}
}

// At the default level a tool storm costs the thread exactly ONE message —
// the ticker, collapsing into its receipt — no matter how many calls stream.
func TestBatchedWriter_DefaultLevelIsOneStatusMessage(t *testing.T) {
	ft := &fakeThread{}
	srv := httptest.NewServer(ft.handler())
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "", "T1", detailsOn, nil)

	const calls = maxToolEntries + 20 // no per-turn entry cap applies here
	ch := make(chan channels.OutboundDelta, calls+1)
	for range calls {
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: "list_pods", Kind: channels.ToolCall}}
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))

	msgs := ft.finalMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, capturedMessage{fmt.Sprintf("🛠️ %d steps · list_pods ×%d", calls, calls)}, msgs[0])
	require.Equal(t, 1, ft.postCount(), "every refresh edits the one ticker message in place")
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
	a.bindThreadAgent("T1", "kagent/sre-agent")
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

// Idle per-thread state (details, usage, resume marks) is swept on insert once
// past threadStateTTL, so a long-lived pod does not accumulate one entry per
// thread forever. synctest fakes time.Now inside the bubble.
func TestThreadState_EvictedAfterTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &Adapter{Logger: slog.New(slog.DiscardHandler)}
		a.setDetailsLevel("T-old", detailsOff)
		a.recordTurnUsage("T-old", "D-old", channels.TurnUsage{TotalTokens: 1})
		a.resumeMu.Lock()
		a.resumeChecked = map[string]time.Time{"T-old": time.Now().Add(threadStateTTL)}
		a.resumeMu.Unlock()

		time.Sleep(threadStateTTL + time.Minute)

		// Inserts sweep the expired entries.
		a.setDetailsLevel("T-new", detailsFull)
		a.recordTurnUsage("T-new", "D-new", channels.TurnUsage{TotalTokens: 2})
		gw := &resumeStub{exists: true, checked: true}
		a.gw = gw
		a.maybeAnnounceResume(t.Context(), channels.InboundMessage{ThreadID: "T-new"}, "D-new")

		a.detailsMu.Lock()
		require.NotContains(t, a.details, "T-old")
		require.Contains(t, a.details, "T-new")
		a.detailsMu.Unlock()

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

// The inactive-thread hint fires only for threads this process has a trace of
// (here: a details setting), at most once per thread; unrelated threads in a
// served channel stay silent.
func TestHintInactiveThread_EngagedOnlyAndOnce(t *testing.T) {
	var ephemerals atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "chat.postEphemeral") {
			ephemerals.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	a := &Adapter{
		APIBase: srv.URL,
		Secrets: Secrets{BotToken: "test-bot-token"}, //nolint:gosec
		Logger:  slog.New(slog.DiscardHandler),
	}

	a.hintInactiveThread(t.Context(), "C1", "T1", "U1")
	require.Equal(t, int32(0), ephemerals.Load(), "a thread with no bot trace must stay silent")

	a.setDetailsLevel("T1", detailsOff)
	a.hintInactiveThread(t.Context(), "C1", "T1", "U1")
	require.Equal(t, int32(1), ephemerals.Load(), "an engaged thread gets the hint")

	a.hintInactiveThread(t.Context(), "C1", "T1", "U2")
	require.Equal(t, int32(1), ephemerals.Load(), "the hint posts once per thread")
}
