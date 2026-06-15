package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func TestKey_RoundTrip(t *testing.T) {
	t.Run("4-part without agent", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: "ctx-1", UserID: "alice", ThreadID: ""}
		s := k.String()
		got, err := store.ParseKey(s)
		require.NoError(t, err)
		require.Equal(t, k, got)
		require.Empty(t, got.Agent)
	})

	t.Run("5-part with agent", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: "ctx-1", UserID: "alice", ThreadID: "", Agent: "worker-b"}
		s := k.String()
		got, err := store.ParseKey(s)
		require.NoError(t, err)
		require.Equal(t, k, got)
		require.Equal(t, "worker-b", got.Agent)
	})

	t.Run("4-part and 5-part with same channel+id differ", func(t *testing.T) {
		k4 := store.Key{Channel: "a2a", ChannelID: "ctx-1", UserID: "alice", ThreadID: ""}
		k5 := store.Key{Channel: "a2a", ChannelID: "ctx-1", UserID: "alice", ThreadID: "", Agent: "worker-a"}
		require.NotEqual(t, k4.String(), k5.String(), "different agents must produce different keys")
	})

	t.Run("two agents produce distinct keys for same context", func(t *testing.T) {
		ka := store.Key{Channel: "a2a", ChannelID: "ctx-1", UserID: "alice", ThreadID: "", Agent: "worker-a"}
		kb := store.Key{Channel: "a2a", ChannelID: "ctx-1", UserID: "alice", ThreadID: "", Agent: "worker-b"}
		require.NotEqual(t, ka.String(), kb.String())
	})

	t.Run("pipe and backslash in agent field survive escape round-trip", func(t *testing.T) {
		k := store.Key{Channel: "a2a", ChannelID: "c", UserID: "u", ThreadID: "", Agent: "work|er\\b"}
		got, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, got)
	})

	t.Run("invalid key rejected", func(t *testing.T) {
		_, err := store.ParseKey("only|three|parts")
		require.Error(t, err)
	})
}
