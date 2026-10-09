package channels

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func TestSynthesizeContextID(t *testing.T) {
	base := func() string {
		return SynthesizeContextID("slack", "C123", "U456", "T789", "worker", nil)
	}

	t.Run("distinct_thread", func(t *testing.T) {
		other := SynthesizeContextID("slack", "C123", "U456", "T999", "worker", nil)
		require.NotEqual(t, base(), other, "different threadID must yield different ID")
	})

	t.Run("distinct_agent", func(t *testing.T) {
		other := SynthesizeContextID("slack", "C123", "U456", "T789", "worker-b", nil)
		require.NotEqual(t, base(), other, "different agentRef must yield different ID")
	})

	t.Run("distinct_channel_type", func(t *testing.T) {
		other := SynthesizeContextID("other", "C123", "U456", "T789", "worker", nil)
		require.NotEqual(t, base(), other, "different channel must yield different ID")
	})

	t.Run("no_prefix_collision", func(t *testing.T) {
		// Without length-prefix encoding, ("ab","c") and ("a","bc") collide.
		a := SynthesizeContextID("ab", "c", "U", "T", "w", nil)
		b := SynthesizeContextID("a", "bc", "U", "T", "w", nil)
		require.NotEqual(t, a, b, "length-prefix encoding must prevent boundary collision")
	})
}

// The key is the Session create's idempotency key: a thread without a
// workspace, or with an explicit none, must keep the key every earlier release
// computed, byte for byte, or it loses its Session. A chosen workspace gets a
// key of its own, the same on every call.
func TestSynthesizeContextID_Workspace(t *testing.T) {
	// printf '5:slack|2:C1|0:|9:1700.0001|13:kagent/worker|' | shasum -a 256
	const today = "84393e8104d7c8645e2f4e69d95afc4613c1c8ebe488e0d64a4c382c0c9a654c"
	// printf '5:slack|2:C1|0:|9:1700.0001|13:kagent/worker|6:kagent|9:klaus-dev|' | shasum -a 256
	const chosen = "0226bb21c6dffaa630bfcf16123a80d25ff88f2f8bd01542eec7f0ecc97c97ec"
	key := func(w *store.WorkspaceChoice) string {
		return SynthesizeContextID("slack", "C1", "", "1700.0001", "kagent/worker", w)
	}

	require.Equal(t, today, key(nil), "no workspace keeps today's key")
	require.Equal(t, today, key(&store.WorkspaceChoice{None: true}), "an explicit none keeps today's key")

	ws := &store.WorkspaceChoice{Namespace: "kagent", Name: "klaus-dev"}
	require.Equal(t, chosen, key(ws), "a chosen workspace has a stable key")
	require.Equal(t, key(ws), key(&store.WorkspaceChoice{Namespace: "kagent", Name: "klaus-dev"}))
	require.NotEqual(t, key(ws), key(&store.WorkspaceChoice{Namespace: "kagent", Name: "other"}))
	require.NotEqual(t, key(ws), key(&store.WorkspaceChoice{Namespace: "kagen", Name: "tklaus-dev"}),
		"length-prefix encoding separates namespace and name")
}
