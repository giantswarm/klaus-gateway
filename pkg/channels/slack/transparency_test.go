package slack

import (
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

func TestCompactJSON_TruncatesAndEmpty(t *testing.T) {
	require.Equal(t, "", compactJSON(nil, 100))
	require.Equal(t, "", compactJSON(map[string]any{}, 100))
	require.Equal(t, `{"a":"b"}`, compactJSON(map[string]any{"a": "b"}, 100))

	out := compactJSON(map[string]any{"k": "0123456789"}, 8)
	require.Len(t, []rune(out), 9, "8 runes + ellipsis")
	require.Contains(t, out, "…")
}
