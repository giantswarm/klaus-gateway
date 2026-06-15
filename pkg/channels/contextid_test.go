package channels

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSynthesizeContextID(t *testing.T) {
	base := func() string {
		return SynthesizeContextID("slack", "C123", "U456", "T789", "worker")
	}

	t.Run("stable", func(t *testing.T) {
		require.Equal(t, base(), base(), "same inputs must yield the same ID")
	})

	t.Run("distinct_thread", func(t *testing.T) {
		other := SynthesizeContextID("slack", "C123", "U456", "T999", "worker")
		require.NotEqual(t, base(), other, "different threadID must yield different ID")
	})

	t.Run("distinct_agent", func(t *testing.T) {
		other := SynthesizeContextID("slack", "C123", "U456", "T789", "worker-b")
		require.NotEqual(t, base(), other, "different agentRef must yield different ID")
	})

	t.Run("distinct_channel_type", func(t *testing.T) {
		other := SynthesizeContextID("cli", "C123", "U456", "T789", "worker")
		require.NotEqual(t, base(), other, "different channel must yield different ID")
	})

	t.Run("no_prefix_collision", func(t *testing.T) {
		// Without length-prefix encoding, ("ab","c") and ("a","bc") collide.
		a := SynthesizeContextID("ab", "c", "U", "T", "w")
		b := SynthesizeContextID("a", "bc", "U", "T", "w")
		require.NotEqual(t, a, b, "length-prefix encoding must prevent boundary collision")
	})
}
