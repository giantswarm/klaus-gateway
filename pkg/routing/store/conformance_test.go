package store_test

import (
	"context"
	"fmt"
	"os"
	"sync"
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
		k := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700000000.000100"}
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

	t.Run("one-row-per-thread", func(t *testing.T) {
		// A thread has one row: the agent it is bound to, the AgentInstance and
		// the task in flight on it, and the channel's initiator and grants.
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelWeb, ChannelID: "C1", ThreadID: "1700000000.000100"}
		now := time.Now().UTC().Truncate(time.Second)
		in := store.Entry{
			AgentRef: "kagent/sre-agent", AgentInstanceID: "i-1", TaskID: "task-7",
			Resume:    map[string]string{"slack_user": "U1"},
			Initiator: "U1", Granted: []string{"U2", "U3"},
			CreatedAt: now, LastSeen: now, TTL: 30 * 24 * time.Hour,
		}
		require.NoError(t, s.Put(ctx, k, in))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, in.AgentRef, got.AgentRef)
		require.Equal(t, in.AgentInstanceID, got.AgentInstanceID)
		require.Equal(t, in.TaskID, got.TaskID)
		require.Equal(t, in.Resume, got.Resume)
		require.Equal(t, in.Initiator, got.Initiator)
		require.Equal(t, in.Granted, got.Granted)
		require.Empty(t, got.Instance)

		entries, err := s.List(ctx)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, k, entries[0].Key)
		require.Equal(t, in.AgentInstanceID, entries[0].Entry.AgentInstanceID)
		require.Equal(t, in.Initiator, entries[0].Entry.Initiator)
	})

	t.Run("update-creates-and-merges", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelWeb, ChannelID: "C1", ThreadID: "T1"}

		require.NoError(t, s.Update(ctx, k, func(e *store.Entry, found bool) bool {
			require.False(t, found)
			require.Equal(t, store.Entry{}, *e)
			e.Instance, e.LastSeen, e.TTL = "i1", time.Now(), time.Hour
			return true
		}))
		require.NoError(t, s.Update(ctx, k, func(e *store.Entry, found bool) bool {
			require.True(t, found)
			require.Equal(t, "i1", e.Instance)
			e.Initiator, e.Granted = "U1", []string{"U2"}
			return true
		}))

		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "i1", got.Instance, "the first writer's field survives the second")
		require.Equal(t, "U1", got.Initiator)
		require.Equal(t, []string{"U2"}, got.Granted)
	})

	t.Run("update-no-write-when-unchanged", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelWeb, ChannelID: "C1", ThreadID: "T1"}

		require.NoError(t, s.Update(ctx, k, func(*store.Entry, bool) bool { return false }))
		_, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.False(t, ok, "a mutate that reports no change creates nothing")

		require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i1", LastSeen: time.Now(), TTL: time.Hour}))
		require.NoError(t, s.Update(ctx, k, func(e *store.Entry, _ bool) bool {
			e.Instance = "other"
			return false
		}))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "i1", got.Instance, "the row is untouched")
	})

	t.Run("update-expired-is-absent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelWeb, ChannelID: "C1", ThreadID: "T1"}
		require.NoError(t, s.Put(ctx, k, store.Entry{
			Instance: "i1", Initiator: "U1",
			CreatedAt: time.Now().Add(-2 * time.Hour), LastSeen: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		}))
		require.NoError(t, s.Update(ctx, k, func(e *store.Entry, found bool) bool {
			require.False(t, found, "an expired entry is absent")
			require.Equal(t, store.Entry{}, *e)
			return false
		}))
	})

	t.Run("update-serialises-writers", func(t *testing.T) {
		// The grant of one user must not erase another's: every writer reads
		// what the previous one wrote.
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: channelWeb, ChannelID: "C1", ThreadID: "T1"}
		const writers = 32
		var wg sync.WaitGroup
		errs := make([]error, writers)
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = s.Update(ctx, k, func(e *store.Entry, _ bool) bool {
					e.Granted = append(e.Granted, fmt.Sprintf("U%02d", i))
					e.LastSeen, e.TTL = time.Now(), time.Hour
					return true
				})
			}()
		}
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Len(t, got.Granted, writers, "no writer lost another's grant")
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
