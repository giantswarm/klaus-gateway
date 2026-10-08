package slack

import (
	"testing"

	"github.com/stretchr/testify/require"
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
