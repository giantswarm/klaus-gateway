package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

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
	require.Equal(t, "Token usage not available yet.", a.usageReport(t.Context(), "T1"))

	a.recordTurnUsage("T1", channels.TurnUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150})
	a.recordTurnUsage("T1", channels.TurnUsage{InputTokens: 30, OutputTokens: 20, TotalTokens: 50})

	report := a.usageReport(t.Context(), "T1")
	require.Contains(t, report, "Last turn — in 30 · out 20 · total 50")
	require.Contains(t, report, "Session — in 130 · out 70 · total 200")

	// An empty turn must not clobber the last-turn figures.
	a.recordTurnUsage("T1", channels.TurnUsage{})
	require.Contains(t, a.usageReport(t.Context(), "T1"), "Last turn — in 30 · out 20 · total 50")
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
	a.recordTurnUsage("T1", channels.TurnUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3})

	report := a.usageReport(t.Context(), "T1")
	require.Contains(t, report, "Model — OpenAI/gpt-5")

	_ = a.usageReport(t.Context(), "T1")
	require.Equal(t, int32(1), source.calls.Load(), "model lookups must be cached")
}

// A BYO agent exposes no model; the line is omitted rather than rendered empty.
func TestUsageReport_OmitsModelLineWhenUnavailable(t *testing.T) {
	a := &Adapter{DefaultAgent: "kagent/sre-agent", Models: &fakeModelSource{}}
	a.recordTurnUsage("T1", channels.TurnUsage{TotalTokens: 3})
	require.NotContains(t, a.usageReport(t.Context(), "T1"), "Model")

	noSource := &Adapter{DefaultAgent: "kagent/sre-agent"}
	noSource.recordTurnUsage("T1", channels.TurnUsage{TotalTokens: 3})
	require.NotContains(t, noSource.usageReport(t.Context(), "T1"), "Model")
}
