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

	// The row lives twice the facade's thread lifetime — the conversation
	// ends after one, the row stays for another so a reply can be told; a
	// second update keeps CreatedAt.
	require.Equal(t, 2*DefaultThreadTTL, rec.TTL)
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

// A thread nobody wrote to for longer than the lifetime is closed: it reads
// as no record at all, ThreadClosed names it and the lifetime it ended after,
// and the next write starts the thread over instead of merging into what the
// store still holds.
func TestThreadRecord_ClosedAfterTheLifetime(t *testing.T) {
	ctx := context.Background()
	mem := memory.New()
	f := &Facade{Routes: mem, ThreadTTL: DefaultThreadTTL}
	now := time.Now()
	clock := func() time.Time { return now }
	f.SetNowFunc(clock)
	mem.SetNowFunc(clock)

	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", func(e *store.Entry, _ bool) bool {
		e.AgentRef, e.AgentInstanceID = "sre-agent", "inst-1"
		e.Initiator, e.Granted = "U1", []string{"U2"}
		return true
	}))
	rec, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2*DefaultThreadTTL, rec.TTL, "the row outlives the conversation by as much again")
	closed, _, err := f.ThreadClosed(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, closed, "a thread within its lifetime is open")

	now = now.Add(DefaultThreadTTL + time.Hour)
	_, ok, err = f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, ok, "past the lifetime the conversation reads as absent")
	closed, lifetime, err := f.ThreadClosed(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, closed)
	require.Equal(t, DefaultThreadTTL, lifetime, "the notice names the lifetime it ended after")

	var found bool
	var handed store.Entry
	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", func(e *store.Entry, seen bool) bool {
		found, handed = seen, *e
		e.Initiator = "U3"
		return true
	}))
	require.False(t, found, "a closed row is handed to the writer as no row")
	require.Zero(t, handed, "and empty, so nothing of the ended conversation is merged into")

	rec, ok, err = f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok, "the write re-opened the thread")
	require.Equal(t, "U3", rec.Initiator)
	require.Empty(t, rec.Granted, "no grant is carried over")
	require.Empty(t, rec.AgentRef)
	require.Empty(t, rec.AgentInstanceID, "the binding is cleared, so the next turn rebinds")
	require.Equal(t, now, rec.CreatedAt, "the thread starts over")
}

// The row itself is dropped by the store at twice the lifetime; from then on
// the thread is a stranger again and nothing is left to tell its author.
func TestThreadRecord_RowGoneAtTwiceTheLifetime(t *testing.T) {
	ctx := context.Background()
	mem := memory.New()
	f := &Facade{Routes: mem, ThreadTTL: DefaultThreadTTL}
	now := time.Now()
	clock := func() time.Time { return now }
	f.SetNowFunc(clock)
	mem.SetNowFunc(clock)

	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", setAgentAndInitiator("sre-agent", "U1")))
	now = now.Add(2*DefaultThreadTTL + time.Hour)
	closed, _, err := f.ThreadClosed(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, closed, "the store dropped the row, so there is nothing to report")
}

// A thread the store never had is not closed — nothing is posted about it.
func TestThreadClosed_UnknownThread(t *testing.T) {
	f := &Facade{Routes: memory.New(), ThreadTTL: DefaultThreadTTL}
	closed, lifetime, err := f.ThreadClosed(context.Background(), "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, closed)
	require.Zero(t, lifetime)
}

// A gateway run with --thread-ttl=0 never closes a conversation, however long
// the thread is silent.
func TestThreadRecord_ZeroTTLNeverCloses(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New(), ThreadTTL: 0}
	now := time.Now()
	f.SetNowFunc(func() time.Time { return now })
	require.NoError(t, f.UpdateThreadRecord(ctx, "slack", "C1", "T1", setAgentAndInitiator("sre-agent", "U1")))

	now = now.Add(10 * 365 * 24 * time.Hour)
	rec, ok, err := f.ThreadRecord(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "U1", rec.Initiator)
	closed, _, err := f.ThreadClosed(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, closed)
}

func TestThreadRecord_NoStore(t *testing.T) {
	f := &Facade{}
	_, _, err := f.ThreadRecord(context.Background(), "slack", "C1", "T1")
	require.Error(t, err)
	require.Error(t, f.UpdateThreadRecord(context.Background(), "slack", "C1", "T1", func(*store.Entry, bool) bool { return true }))
}
