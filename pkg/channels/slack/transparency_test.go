package slack

import (
	"net/http"
	"net/http/httptest"
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
	require.Equal(t, "Token usage not available yet.", a.usageReport("T1"))

	a.recordTurnUsage("T1", channels.TurnUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150})
	a.recordTurnUsage("T1", channels.TurnUsage{InputTokens: 30, OutputTokens: 20, TotalTokens: 50})

	report := a.usageReport("T1")
	require.Contains(t, report, "Last turn — in 30 · out 20 · total 50")
	require.Contains(t, report, "Session — in 130 · out 70 · total 200")

	// An empty turn must not clobber the last-turn figures.
	a.recordTurnUsage("T1", channels.TurnUsage{})
	require.Contains(t, a.usageReport("T1"), "Last turn — in 30 · out 20 · total 50")
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

func TestCompactJSON_TruncatesAndEmpty(t *testing.T) {
	require.Equal(t, "", compactJSON(nil, 100))
	require.Equal(t, "", compactJSON(map[string]any{}, 100))
	require.Equal(t, `{"a":"b"}`, compactJSON(map[string]any{"a": "b"}, 100))

	out := compactJSON(map[string]any{"k": "0123456789"}, 8)
	require.Len(t, []rune(out), 9, "8 runes + ellipsis")
	require.Contains(t, out, "…")
}
