package slack

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
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

// TestBatchedWriter_CapsToolActivity verifies a tool-heavy turn does not flood
// the thread: at most maxToolMessages tool posts plus one truncation note.
func TestBatchedWriter_CapsToolActivity(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1"}`))
	}))
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "", "T1", detailsOn, nil)

	ch := make(chan channels.OutboundDelta, 20)
	for range 15 {
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: "list_pods", Kind: channels.ToolCall}}
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))
	require.Equal(t, int32(maxToolMessages+1), posts.Load(), "10 tool posts + 1 truncation note")
}

// TestBatchedWriter_ToolPostsPreserveOrder verifies the async poster keeps tool
// messages in stream order and drains them all before run() returns.
func TestBatchedWriter_ToolPostsPreserveOrder(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		if text, ok := v["text"].(string); ok {
			mu.Lock()
			got = append(got, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1"}`))
	}))
	t.Cleanup(srv.Close)

	w := newBatchedWriterWithClient(&slackAPIClient{baseURL: srv.URL}, "C1", "", "T1", detailsOn, nil)

	names := []string{"alpha", "bravo", "charlie", "delta"}
	ch := make(chan channels.OutboundDelta, len(names)+1)
	for _, n := range names {
		ch <- channels.OutboundDelta{Kind: channels.DeltaToolActivity, Tool: &channels.ToolActivity{Name: n, Kind: channels.ToolCall}}
	}
	ch <- channels.OutboundDelta{Done: true}
	close(ch)

	require.NoError(t, w.run(t.Context(), ch))

	// run() drained the poster before returning: every post is already recorded.
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, len(names))
	for i, n := range names {
		require.Contains(t, got[i], "`"+n+"`", "tool posts must stay in stream order")
	}
}

func TestCompactJSON_TruncatesAndEmpty(t *testing.T) {
	require.Equal(t, "", compactJSON(nil, 100))
	require.Equal(t, "", compactJSON(map[string]any{}, 100))
	require.Equal(t, `{"a":"b"}`, compactJSON(map[string]any{"a": "b"}, 100))

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
