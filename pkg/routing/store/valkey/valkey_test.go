package valkey_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/storetest"
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
// value is the same JSON the other stores hold: one row per thread, with the
// agent it is bound to, its AgentInstance, the task in flight and the
// channel's initiator and grants.
func TestKeyLayout(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()

	k := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700000000.000100"}
	now := time.Now().Truncate(time.Second)
	e := store.Entry{
		AgentRef: "kagent/sre-agent", AgentInstanceID: "0192f1c2-7d1e-7a3b-9c4d-5e6f7a8b9c0d", TaskID: "task-1",
		Resume:    map[string]string{"slack_user": "U1", "message_ts": "1700000000.000200"},
		Initiator: "U1", Granted: []string{"U2"},
		CreatedAt: now, LastSeen: now,
	}
	require.NoError(t, storetest.Put(ctx, s, k, e))

	require.Equal(t, []string{"klaus-gateway:route:slack|C1|1700000000.000100"}, m.Keys())
	raw, err := m.Get("klaus-gateway:route:slack|C1|1700000000.000100")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Equal(t, e.AgentRef, got["agent_ref"])
	require.Equal(t, e.AgentInstanceID, got["agent_instance_id"])
	require.Equal(t, "task-1", got["task_id"])
	require.Equal(t, map[string]any{"slack_user": "U1", "message_ts": "1700000000.000200"}, got["resume"])
	require.Equal(t, "U1", got["initiator"])
	require.Equal(t, []any{"U2"}, got["granted"])
	require.NotContains(t, got, "thread", "the thread's fields are the row's own, not a nested record")
	require.Zero(t, m.TTL(got0(m)), "a row without TTL never expires")

	back, ok, err := s.Get(ctx, k)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, e.TaskID, back.TaskID)
	require.Equal(t, e.Resume, back.Resume)
	require.Equal(t, e.Granted, back.Granted)
	require.True(t, e.LastSeen.Equal(back.LastSeen))
}

func got0(m *miniredis.Miniredis) string { return m.Keys()[0] }

func TestKeyPrefixOption(t *testing.T) {
	m := miniredis.RunT(t)
	s, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), KeyPrefix: "other[1]:", Timeout: testTimeout})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	k := store.Key{Channel: "slack", ChannelID: "c", ThreadID: "t"}
	require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i"}))
	require.Equal(t, []string{"other[1]:slack|c|t"}, m.Keys())
	// A foreign key next to ours is not listed: the glob metacharacters in
	// the prefix are matched literally.
	require.NoError(t, m.Set("other1:slack|x|y", "{}"))
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
	k := store.Key{Channel: "slack", ChannelID: "c", ThreadID: "t"}

	now := time.Now()
	require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i", LastSeen: now, TTL: time.Hour}))
	require.InDelta(t, time.Hour, m.TTL(got0(m)), float64(2*time.Second))

	require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i", LastSeen: now.Add(-40 * time.Minute), TTL: time.Hour}))
	require.InDelta(t, 20*time.Minute, m.TTL(got0(m)), float64(2*time.Second), "what is left of the TTL from LastSeen")

	require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i", LastSeen: now.Add(time.Hour), TTL: time.Hour}))
	require.InDelta(t, time.Hour, m.TTL(got0(m)), float64(2*time.Second), "a future LastSeen does not extend the TTL")

	m.FastForward(2 * time.Hour)
	_, ok, err := s.Get(ctx, k)
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, m.Keys(), "the server dropped the key")
}

// A review lives next to the routing keys under its own prefix — the routing
// SCAN never reads it — with what is left of its TTL as the key's expiry.
func TestReviewKeyLayout(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	posted := time.Now().Add(-24 * time.Hour)
	r := store.Review{
		ID: "8f3c2d", Channel: "C1", TS: "1700000000.000100", Team: "team-bumblebee", Text: "*Archive* it.",
		Link: "https://github.com/giantswarm/github/pull/4711", Tool: "x_giantswarm-repo-manager_approve_change",
		Arguments: map[string]any{"pr": float64(4711)}, PostedAt: posted, TTL: 7 * 24 * time.Hour,
	}
	require.NoError(t, s.PutReview(ctx, r))
	require.Equal(t, []string{"klaus-gateway:review:8f3c2d"}, m.Keys())
	require.InDelta(t, 6*24*time.Hour, m.TTL("klaus-gateway:review:8f3c2d"), float64(2*time.Second), "what is left of the seven days")

	raw, err := m.Get("klaus-gateway:review:8f3c2d")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Equal(t, "team-bumblebee", got["team"])
	require.Equal(t, "x_giantswarm-repo-manager_approve_change", got["tool"])
	require.Equal(t, map[string]any{"pr": float64(4711)}, got["arguments"])
	require.NotContains(t, got, "decided_by", "an open review carries no decider")
	require.NotContains(t, got, "claimed_at")

	entries, err := s.List(ctx)
	require.NoError(t, err)
	require.Empty(t, entries, "the routing listing does not see reviews")

	m.FastForward(7 * 24 * time.Hour)
	_, ok, err := s.GetReview(ctx, "8f3c2d")
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, m.Keys(), "the server dropped the key")
}

// The review prefix follows the routing prefix, so a gateway that moves its
// keys moves its reviews with them.
func TestReviewKeyPrefixFollowsRoutePrefix(t *testing.T) {
	for routes, want := range map[string]string{
		"":                            "klaus-gateway:review:",
		"other[1]:":                   "other[1]:review:",
		"team-a:klaus-gateway:route:": "team-a:klaus-gateway:review:",
	} {
		m := miniredis.RunT(t)
		s, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), KeyPrefix: routes, Timeout: testTimeout})
		require.NoError(t, err)
		require.NoError(t, s.PutReview(context.Background(), store.Review{ID: "r", PostedAt: time.Now(), TTL: time.Hour}))
		require.Equal(t, []string{want + "r"}, m.Keys(), "routing prefix %q", routes)
		_ = s.Close()
	}
	m := miniredis.RunT(t)
	s, err := valkeystore.New(valkeystore.Options{URL: m.Addr(), ReviewKeyPrefix: "elsewhere:", Timeout: testTimeout})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.PutReview(context.Background(), store.Review{ID: "r", PostedAt: time.Now(), TTL: time.Hour}))
	require.Equal(t, []string{"elsewhere:r"}, m.Keys(), "an explicit review prefix stands")
}

// An update that finds the record changed under it — another replica's click
// landed between the read and the write — is not written over the change: the
// record is re-read and mutate runs again on what is there now.
func TestReviewUpdateIsACompareAndSet(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	require.NoError(t, s.PutReview(ctx, store.Review{ID: "r1", Team: "t", PostedAt: time.Now(), TTL: time.Hour}))

	calls := 0
	found, err := s.UpdateReview(ctx, "r1", func(r *store.Review) bool {
		calls++
		if calls == 1 {
			// The other replica claims the review after this read: rewrite the
			// key underneath, keeping its expiry, the way its own update would.
			raw, err := m.Get("klaus-gateway:review:r1")
			require.NoError(t, err)
			var other store.Review
			require.NoError(t, json.Unmarshal([]byte(raw), &other))
			other.DecidedBy = "U-other"
			buf, err := json.Marshal(other)
			require.NoError(t, err)
			require.NoError(t, m.Set("klaus-gateway:review:r1", string(buf)))
		}
		if r.DecidedBy != "" {
			return false // told who holds it; nothing to write
		}
		r.DecidedBy = "U-mine"
		return true
	})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 2, calls, "the first write was refused and mutate ran again on the current record")
	got, ok, err := s.GetReview(ctx, "r1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "U-other", got.DecidedBy, "the other replica's claim stands")
}

// Two stores on one server — two gateway replicas — race for one review:
// exactly one claim lands.
func TestReviewClaimAcrossReplicas(t *testing.T) {
	m := miniredis.RunT(t)
	replicas := []*valkeystore.Store{newStore(t, m.Addr()), newStore(t, m.Addr())}
	ctx := context.Background()
	require.NoError(t, replicas[0].PutReview(ctx, store.Review{ID: "r1", PostedAt: time.Now(), TTL: time.Hour}))

	const perReplica = 16
	var wg sync.WaitGroup
	var wins atomic.Int32
	for ri, s := range replicas {
		for i := range perReplica {
			wg.Add(1)
			go func() {
				defer wg.Done()
				user := fmt.Sprintf("R%dU%02d", ri, i)
				won := false
				_, err := s.UpdateReview(ctx, "r1", func(r *store.Review) bool {
					won = false
					if r.DecidedBy != "" {
						return false
					}
					r.DecidedBy, won = user, true
					return true
				})
				require.NoError(t, err)
				if won {
					wins.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	require.Equal(t, int32(1), wins.Load())
}

func TestPutExpiredEntryDeletes(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	k := store.Key{Channel: "slack", ChannelID: "c", ThreadID: "t"}
	require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i", LastSeen: time.Now()}))
	require.Len(t, m.Keys(), 1)
	require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i", LastSeen: time.Now().Add(-2 * time.Hour), TTL: time.Hour}))
	require.Empty(t, m.Keys())
}

func TestListScansPastOnePage(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	const n = 700 // more than one SCAN page and more than one MGET batch
	for i := range n {
		k := store.Key{Channel: "slack", ChannelID: "C", ThreadID: time.Unix(int64(i), 0).Format("1136239445.000000")}
		require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "a", LastSeen: time.Now()}))
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
	k := store.Key{Channel: "slack", ChannelID: "c", ThreadID: "t"}
	require.NoError(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i", LastSeen: time.Now()}))

	m.Close()
	start := time.Now()
	_, _, err := s.Get(ctx, k)
	require.Error(t, err)
	require.Less(t, time.Since(start), 4*testTimeout, "an unreachable server fails the turn fast")
	require.Error(t, storetest.Put(ctx, s, k, store.Entry{AgentInstanceID: "i", LastSeen: time.Now()}))

	require.NoError(t, m.Restart())
	require.Eventually(t, func() bool { return s.Ping(ctx) == nil }, 5*time.Second, 50*time.Millisecond)
	got, ok, err := s.Get(ctx, k)
	require.NoError(t, err)
	require.True(t, ok, "miniredis keeps its data across Restart")
	require.Equal(t, "i", got.AgentInstanceID)
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
	_, _, err = s.Get(context.Background(), store.Key{Channel: "slack", ChannelID: "c", ThreadID: "t"})
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*testTimeout)
}

func TestClosedStoreRefuses(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	require.NoError(t, s.Close())
	_, _, err := s.Get(context.Background(), store.Key{Channel: "slack", ChannelID: "c", ThreadID: "t"})
	require.Error(t, err)
	require.NoError(t, s.Close(), "Close is idempotent")
}

// A restart with every pipe of the client dialed: the readiness PING heals
// only the pipe it runs on, so a keyed command must not fail on another pipe
// that still holds a dead connection (klaus-gateway#261).
func TestRestartWithEveryPipeDialed(t *testing.T) {
	m := miniredis.RunT(t)
	s := newStore(t, m.Addr())
	ctx := context.Background()
	key := func(i int) store.Key {
		return store.Key{Channel: "slack", ChannelID: "c", ThreadID: fmt.Sprint(i)}
	}
	for i := 0; i < 40; i++ {
		require.NoError(t, storetest.Put(ctx, s, key(i), store.Entry{AgentInstanceID: "i", LastSeen: time.Now()}))
	}
	m.Close()
	require.NoError(t, m.Restart())
	require.Eventually(t, func() bool { return s.Ping(ctx) == nil }, 5*time.Second, 50*time.Millisecond)
	for i := 0; i < 20; i++ {
		_, ok, err := s.Get(ctx, key(i))
		require.NoError(t, err)
		require.True(t, ok)
	}
}
