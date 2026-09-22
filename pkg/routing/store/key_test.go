package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func TestKey_RoundTrip(t *testing.T) {
	t.Run("three parts", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: "ctx-1", ThreadID: ""}
		got, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, got)
	})

	t.Run("pipe and backslash survive the escape round-trip", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: `c|1\2`, ThreadID: "t"}
		got, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, got)
	})

	t.Run("invalid key rejected", func(t *testing.T) {
		_, err := store.ParseKey("only|two")
		require.Error(t, err)
		// The 4-part key of the user-slot layout is no longer a routing key: a
		// row written by an older gateway is skipped, not misread.
		_, err = store.ParseKey("slack|C1||1700000000.000100")
		require.Error(t, err)
	})
}
