package channels

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// setAgentAndInitiator is the write a channel adapter makes when a thread
// opens: the agent it selected and the user who launched it.
func setAgentAndInitiator(agentRef, initiator string) func(*store.Entry, bool) bool {
	return func(e *store.Entry, _ bool) bool {
		e.AgentRef, e.Initiator = agentRef, initiator
		return true
	}
}

func TestThreadRecord_SaveAndLoad(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New(), ThreadTTL: DefaultThreadTTL}
	_, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", setAgentAndInitiator("sre-agent", "U1")))
	rec, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "sre-agent", rec.AgentRef)
	require.Equal(t, "U1", rec.Initiator)
	require.WithinDuration(t, time.Now(), rec.LastSeen, time.Minute)

	// The row carries the facade's thread lifetime; a second update keeps
	// CreatedAt.
	require.Equal(t, DefaultThreadTTL, rec.TTL)
	created := rec.CreatedAt
	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", func(e *store.Entry, _ bool) bool {
		e.Granted = append(e.Granted, "U2")
		return true
	}))
	rec, _, _ = f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.Equal(t, created, rec.CreatedAt)
	require.Equal(t, []string{"U2"}, rec.Granted)
	require.Equal(t, "sre-agent", rec.AgentRef, "the earlier fields survive")
}

// A mutate that reports no change writes nothing.
func TestThreadRecord_UnchangedWritesNothing(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New(), ThreadTTL: DefaultThreadTTL}
	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", func(*store.Entry, bool) bool { return false }))
	_, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, ok)
}

// A gateway run with --thread-ttl=0 writes rows that never expire.
func TestThreadRecord_ZeroTTLNeverExpires(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New(), ThreadTTL: 0}
	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", setAgentAndInitiator("sre-agent", "U1")))
	e, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Zero(t, e.TTL)
}

// On the Klaus-instance path the router keeps the thread's instance route on
// the same row; the channel's own fields merge into it, they do not replace it.
func TestThreadRecord_UpdateMergesIntoAnInstanceRoute(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New(), ThreadTTL: DefaultThreadTTL}
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T1"}
	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, f.Routes.Put(ctx, key, store.Entry{Instance: "klaus-1", CreatedAt: created, LastSeen: created, TTL: 24 * time.Hour}))

	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", setAgentAndInitiator("sre-agent", "U1")))
	e, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "klaus-1", e.Instance, "the route's instance survives the record write")
	require.Equal(t, created, e.CreatedAt)
	require.Equal(t, 24*time.Hour, e.TTL, "a TTL the router set is kept")
	require.WithinDuration(t, time.Now(), e.LastSeen, time.Minute)
	require.Equal(t, "U1", e.Initiator)
}

func TestThreadRecord_NoStore(t *testing.T) {
	f := &Facade{}
	_, _, err := f.ThreadRecord(context.Background(), "slack", "C1", "T1")
	require.Error(t, err)
	require.Error(t, f.UpdateThreadRecord(context.Background(), "slack", "C1", "T1", func(*store.Entry, bool) bool { return true }))
}
