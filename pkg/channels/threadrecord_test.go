package channels

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

func TestThreadRecord_SaveAndLoad(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New(), ThreadTTL: DefaultThreadTTL}
	_, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, f.SaveThreadRecord(ctx, "slack", "C1", "T1", store.Thread{AgentRef: "sre-agent", Initiator: "U1"}))
	rec, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "sre-agent", rec.AgentRef)
	require.Equal(t, "U1", rec.Initiator)
	require.WithinDuration(t, time.Now(), rec.LastSeen, time.Minute)

	// The record carries the facade's thread lifetime; a second save keeps
	// CreatedAt.
	e, _, _ := f.Routes.Get(ctx, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T1"})
	require.Equal(t, DefaultThreadTTL, e.TTL)
	created := e.CreatedAt
	require.NoError(t, f.SaveThreadRecord(ctx, "slack", "C1", "T1", store.Thread{AgentRef: "sre-agent", Initiator: "U1", Granted: []string{"U2"}}))
	e, _, _ = f.Routes.Get(ctx, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T1"})
	require.Equal(t, created, e.CreatedAt)
	require.Equal(t, []string{"U2"}, e.Thread.Granted)
}

// A gateway run with --thread-ttl=0 writes records that never expire.
func TestThreadRecord_ZeroTTLNeverExpires(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New(), ThreadTTL: 0}
	require.NoError(t, f.SaveThreadRecord(ctx, "slack", "C1", "T1", store.Thread{AgentRef: "sre-agent", Initiator: "U1"}))
	e, ok, err := f.Routes.Get(ctx, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T1"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Zero(t, e.TTL)
}

// On the Klaus-instance path the router keeps the thread's instance route at
// the same 4-part key; a thread record must merge into it, not replace it.
func TestThreadRecord_SaveMergesIntoAnInstanceRoute(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New(), ThreadTTL: DefaultThreadTTL}
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T1"}
	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, f.Routes.Put(ctx, key, store.Entry{Instance: "klaus-1", CreatedAt: created, LastSeen: created, TTL: 24 * time.Hour}))

	require.NoError(t, f.SaveThreadRecord(ctx, "slack", "C1", "T1", store.Thread{AgentRef: "sre-agent", Initiator: "U1"}))
	e, ok, err := f.Routes.Get(ctx, key)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "klaus-1", e.Instance, "the route's instance survives the record write")
	require.Equal(t, created, e.CreatedAt)
	require.Equal(t, 24*time.Hour, e.TTL, "a TTL the router set is kept")
	require.WithinDuration(t, time.Now(), e.LastSeen, time.Minute)
	require.NotNil(t, e.Thread)
	require.Equal(t, "U1", e.Thread.Initiator)
}

func TestThreadRecord_NoStore(t *testing.T) {
	f := &Facade{}
	_, _, err := f.ThreadRecord(context.Background(), "slack", "C1", "T1")
	require.Error(t, err)
}
