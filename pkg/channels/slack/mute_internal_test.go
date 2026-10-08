package slack

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// Slack timestamps order by their seconds, then by their microseconds read as
// a number; one that does not parse is never later.
func TestTSAfter(t *testing.T) {
	for _, tc := range []struct {
		ts, ref string
		want    bool
	}{
		{"1700000000.000200", "1700000000.000100", true},
		{"1700000000.000100", "1700000000.000100", false},
		{"1700000000.000100", "1700000000.000200", false},
		{"1700000001.000000", "1700000000.999999", true},
		{"500.010", "500.001", true},
		{"500.1", "500.010", true},
		{"500", "499.999999", true},
		{"", "500.001", false},
		{"500.001", "bogus", false},
		{"500.0000001", "500.000", false},
	} {
		require.Equal(t, tc.want, tsAfter(tc.ts, tc.ref), "tsAfter(%q, %q)", tc.ts, tc.ref)
	}
}

// A message written before the mute does not end it when it reaches a turn
// later — one that passed the gate just before the mute was written, or one
// a sign-in or an Allow took just before the mute dropped it; a later one
// ends it and returns the mute's ts.
func TestEndMute_EarlierMessageKeepsTheMute(t *testing.T) {
	gw := &fakeGateway{Facade: newMemoryRecorder()}
	a, _ := newDecisionAdapter(t, gw, nil)
	require.NoError(t, gw.UpdateThreadRecord(t.Context(), ChannelName, "C001", "T001", func(e *store.Entry, _ bool) bool {
		e.MutedAt = "500.002"
		return true
	}))
	mutedAt := func() string {
		row, _, err := gw.ThreadRecord(t.Context(), ChannelName, "C001", "T001")
		require.NoError(t, err)
		return row.MutedAt
	}

	require.Empty(t, a.endMute(t.Context(), "C001", "T001", "500.001"))
	require.Equal(t, "500.002", mutedAt(), "an earlier message keeps the mute")

	require.Equal(t, "500.002", a.endMute(t.Context(), "C001", "T001", "500.003"))
	require.Empty(t, mutedAt(), "a later message ends it")
}
