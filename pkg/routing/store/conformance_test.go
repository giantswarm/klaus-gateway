package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	boltstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/bolt"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
	valkeystore "github.com/giantswarm/klaus-gateway/pkg/routing/store/valkey"
)

const channelWeb = "web"

// runConformance exercises the Store contract. Every backend must pass.
func runConformance(t *testing.T, factory func(t *testing.T) store.Store) {
	t.Helper()

	t.Run("put-get-delete", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelWeb, ChannelID: "c1", UserID: "u1", ThreadID: "t1"}
		e := store.Entry{Instance: "i1", CreatedAt: time.Now(), LastSeen: time.Now(), TTL: time.Hour}

		_, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.False(t, ok)

		require.NoError(t, s.Put(ctx, k, e))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, e.Instance, got.Instance)

		require.NoError(t, s.Delete(ctx, k))
		_, ok, err = s.Get(ctx, k)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("list", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		keys := []store.Key{
			{Channel: channelWeb, ChannelID: "c1", UserID: "u1", ThreadID: "t1"},
			{Channel: "slack", ChannelID: "c2", UserID: "u2", ThreadID: "t2"},
		}
		for i, k := range keys {
			require.NoError(t, s.Put(ctx, k, store.Entry{
				Instance: "inst", CreatedAt: time.Now(), LastSeen: time.Now(), TTL: time.Hour,
			}))
			_ = i
		}
		entries, err := s.List(ctx)
		require.NoError(t, err)
		require.Len(t, entries, 2)
	})

	t.Run("keys-with-pipes-round-trip", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelWeb, ChannelID: "c|pipe", UserID: "u1", ThreadID: `t\back`}
		require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "inst", LastSeen: time.Now(), TTL: time.Hour}))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "inst", got.Instance)
	})

	t.Run("agent-instance-round-trip", func(t *testing.T) {
		// A kagent conversation binds the thread to an AgentInstance instead of a
		// Klaus instance; the id must survive the backend's serialisation with
		// the Klaus instance name left empty.
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700000000.000100", Agent: "kagent/sre-agent"}
		e := store.Entry{AgentInstanceID: "0192f1c2-7d1e-7a3b-9c4d-5e6f7a8b9c0d", CreatedAt: time.Now(), LastSeen: time.Now()}

		require.NoError(t, s.Put(ctx, k, e))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, e.AgentInstanceID, got.AgentInstanceID)
		require.Empty(t, got.Instance)

		entries, err := s.List(ctx)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, k, entries[0].Key)
		require.Equal(t, e.AgentInstanceID, entries[0].Entry.AgentInstanceID)
	})

	t.Run("ttl-expired-filtered", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelWeb, ChannelID: "c1", UserID: "u1", ThreadID: "t1"}
		e := store.Entry{
			Instance:  "i1",
			CreatedAt: time.Now().Add(-2 * time.Hour),
			LastSeen:  time.Now().Add(-2 * time.Hour),
			TTL:       time.Hour,
		}
		require.NoError(t, s.Put(ctx, k, e))
		_, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.False(t, ok, "expired entry should not be returned")

		entries, err := s.List(ctx)
		require.NoError(t, err)
		for _, kv := range entries {
			require.NotEqual(t, "i1", kv.Entry.Instance)
		}
	})
}

func TestMemoryStore_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) store.Store {
		s := memory.New()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestBoltStore_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) store.Store {
		path := t.TempDir() + "/routes.bolt"
		s, err := boltstore.Open(path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// The Valkey store is exercised against a server speaking the real protocol
// over TCP (miniredis), not a fake client: a fake validates nothing about the
// wire format.
func TestValkeyStore_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) store.Store {
		m := miniredis.RunT(t)
		s, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), Timeout: time.Second})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// TestValkeyStore_ConformanceReal runs the same contract against a real Valkey
// named by KLAUS_GATEWAY_TEST_VALKEY_URL (host:port; password in
// KLAUS_GATEWAY_TEST_VALKEY_PASSWORD), for the cases where miniredis and the
// server diverge. Every run uses its own key prefix and cleans up after itself.
func TestValkeyStore_ConformanceReal(t *testing.T) {
	url := os.Getenv("KLAUS_GATEWAY_TEST_VALKEY_URL")
	if url == "" {
		t.Skip("KLAUS_GATEWAY_TEST_VALKEY_URL not set")
	}
	runConformance(t, func(t *testing.T) store.Store {
		s, err := valkeystore.New(valkeystore.Options{
			URL:       url,
			Password:  os.Getenv("KLAUS_GATEWAY_TEST_VALKEY_PASSWORD"),
			KeyPrefix: "klaus-gateway-test:" + t.Name() + ":" + time.Now().Format("150405.000") + ":",
			Timeout:   2 * time.Second,
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx := context.Background()
			entries, err := s.List(ctx)
			require.NoError(t, err)
			for _, ke := range entries {
				require.NoError(t, s.Delete(ctx, ke.Key))
			}
			_ = s.Close()
		})
		return s
	})
}

func TestKey_StringRoundTrip(t *testing.T) {
	cases := []store.Key{
		{Channel: channelWeb, ChannelID: "abc", UserID: "u1", ThreadID: "t1"},
		{Channel: "slack", ChannelID: "C|123", UserID: `user\1`, ThreadID: ""},
	}
	for _, k := range cases {
		parsed, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, parsed)
	}
}
