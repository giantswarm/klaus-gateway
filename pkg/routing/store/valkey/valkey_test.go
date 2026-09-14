package valkey_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	valkeystore "github.com/giantswarm/klaus-gateway/pkg/routing/store/valkey"
)

const testTimeout = 300 * time.Millisecond

func newStore(t *testing.T, addr string) *valkeystore.Store {
	t.Helper()
	s, err := valkeystore.New(valkeystore.Options{URL: addr, Timeout: testTimeout})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNew_RequiresURL(t *testing.T) {
	_, err := valkeystore.New(valkeystore.Options{})
	require.Error(t, err)
}

// The key layout is the contract the issue fixes: prefix + Key.String(), so a
// channel's entries share one prefix (klaus-gateway:route:slack|…) and the
// value is the same JSON the other stores hold.
func TestKeyLayout(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()

	k := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700000000.000100", Agent: "kagent/sre-agent"}
	now := time.Now().Truncate(time.Second)
	e := store.Entry{AgentInstanceID: "0192f1c2-7d1e-7a3b-9c4d-5e6f7a8b9c0d", TaskID: "task-1",
		Resume: map[string]string{"slack_user": "U1", "message_ts": "1700000000.000200"}, CreatedAt: now, LastSeen: now}
	require.NoError(t, s.Put(ctx, k, e))

	require.Equal(t, []string{"klaus-gateway:route:slack|C1||1700000000.000100|kagent/sre-agent"}, m.Keys())
	raw, err := m.Get("klaus-gateway:route:slack|C1||1700000000.000100|kagent/sre-agent")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Equal(t, e.AgentInstanceID, got["agent_instance_id"])
	require.Equal(t, "task-1", got["task_id"])
	require.Equal(t, map[string]any{"slack_user": "U1", "message_ts": "1700000000.000200"}, got["resume"])
	require.NotContains(t, got, "instance", "an empty Klaus instance is omitted, as in the other stores")
	require.Zero(t, m.TTL(got0(m)), "a binding without TTL never expires")

	back, ok, err := s.Get(ctx, k)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, e.TaskID, back.TaskID)
	require.Equal(t, e.Resume, back.Resume)
	require.True(t, e.LastSeen.Equal(back.LastSeen))
}

func got0(m *miniredis.Miniredis) string { return m.Keys()[0] }

func TestKeyPrefixOption(t *testing.T) {
	m := miniredis.RunT(t)
	s, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), KeyPrefix: "other[1]:", Timeout: testTimeout})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	k := store.Key{Channel: "web", ChannelID: "c", ThreadID: "t"}
	require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i"}))
	require.Equal(t, []string{"other[1]:web|c||t"}, m.Keys())
	// A foreign key next to ours is not listed: the glob metacharacters in
	// the prefix are matched literally.
	require.NoError(t, m.Set("other1:web|x||y", "{}"))
	entries, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, k, entries[0].Key)
}

// The TTL is the key's expiry: counted from LastSeen, refreshed by every Put,
// enforced by the server rather than by a sweep.
func TestTTLIsServerSide(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	k := store.Key{Channel: "web", ChannelID: "c", UserID: "u", ThreadID: "t"}

	now := time.Now()
	require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i", LastSeen: now, TTL: time.Hour}))
	require.InDelta(t, time.Hour, m.TTL(got0(m)), float64(2*time.Second))

	require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i", LastSeen: now.Add(-40 * time.Minute), TTL: time.Hour}))
	require.InDelta(t, 20*time.Minute, m.TTL(got0(m)), float64(2*time.Second), "what is left of the TTL from LastSeen")

	require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i", LastSeen: now.Add(time.Hour), TTL: time.Hour}))
	require.InDelta(t, time.Hour, m.TTL(got0(m)), float64(2*time.Second), "a future LastSeen does not extend the TTL")

	m.FastForward(2 * time.Hour)
	_, ok, err := s.Get(ctx, k)
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, m.Keys(), "the server dropped the key")
}

func TestPutExpiredEntryDeletes(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	k := store.Key{Channel: "web", ChannelID: "c", UserID: "u", ThreadID: "t"}
	require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i", LastSeen: time.Now()}))
	require.Len(t, m.Keys(), 1)
	require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i", LastSeen: time.Now().Add(-2 * time.Hour), TTL: time.Hour}))
	require.Empty(t, m.Keys())
}

func TestListScansPastOnePage(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	const n = 700 // more than one SCAN page and more than one MGET batch
	for i := range n {
		k := store.Key{Channel: "slack", ChannelID: "C", ThreadID: time.Unix(int64(i), 0).Format("1136239445.000000")}
		require.NoError(t, s.Put(ctx, k, store.Entry{AgentInstanceID: "a", LastSeen: time.Now()}))
	}
	entries, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, entries, n)
}

func TestPing(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	require.NoError(t, s.Ping(context.Background()))
	m.Close()
	require.Error(t, s.Ping(context.Background()))
}

func TestAuth(t *testing.T) {
	m := miniredis.RunT(t)
	m.RequireUserAuth("gateway", "s3cret")
	s, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), Username: "gateway", Password: "s3cret", Timeout: testTimeout})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.Ping(context.Background()))

	wrong, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), Username: "gateway", Password: "nope", Timeout: testTimeout})
	require.NoError(t, err)
	t.Cleanup(func() { _ = wrong.Close() })
	require.Error(t, wrong.Ping(context.Background()))
}

// An outage fails the operation within the timeout and is survived without
// reopening the store: the next call after the server is back succeeds.
func TestOutageFailsFastAndRecovers(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	k := store.Key{Channel: "web", ChannelID: "c", UserID: "u", ThreadID: "t"}
	require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i", LastSeen: time.Now()}))

	m.Close()
	start := time.Now()
	_, _, err := s.Get(ctx, k)
	require.Error(t, err)
	require.Less(t, time.Since(start), 4*testTimeout, "an unreachable server fails the turn fast")
	require.Error(t, s.Put(ctx, k, store.Entry{Instance: "i", LastSeen: time.Now()}))

	require.NoError(t, m.Restart())
	require.Eventually(t, func() bool { return s.Ping(ctx) == nil }, 5*time.Second, 50*time.Millisecond)
	got, ok, err := s.Get(ctx, k)
	require.NoError(t, err)
	require.True(t, ok, "miniredis keeps its data across Restart")
	require.Equal(t, "i", got.Instance)
}

// A server that accepts the connection and never answers (a half-open node, a
// black-holing network policy) is the case that hangs a thread: the command
// must time out.
func TestHangingServerTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
		}
	}()
	s := newStore(t, ln.Addr().String())
	start := time.Now()
	_, _, err = s.Get(context.Background(), store.Key{Channel: "web", ChannelID: "c", UserID: "u", ThreadID: "t"})
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*testTimeout)
}

func TestClosedStoreRefuses(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	require.NoError(t, s.Close())
	_, _, err := s.Get(context.Background(), store.Key{Channel: "web", ChannelID: "c", UserID: "u", ThreadID: "t"})
	require.Error(t, err)
	require.NoError(t, s.Close(), "Close is idempotent")
}
