package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func TestKey_RoundTrip(t *testing.T) {
	t.Run("four parts", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: "ctx-1", UserID: "alice", ThreadID: ""}
		got, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, got)
	})

	t.Run("pipe and backslash survive the escape round-trip", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: `c|1\2`, UserID: "u", ThreadID: "t"}
		got, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, got)
	})

	t.Run("invalid key rejected", func(t *testing.T) {
		_, err := store.ParseKey("only|three|parts")
		require.Error(t, err)
		// The 5-part key of the agent-slot layout is no longer a routing key.
		_, err = store.ParseKey("slack|C1||1700000000.000100|kagent/sre-agent")
		require.Error(t, err)
	})
}
