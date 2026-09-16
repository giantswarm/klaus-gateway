package channels

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

func TestThreadRecord_SaveLoadFind(t *testing.T) {
	ctx := context.Background()
	f := &Facade{Routes: memory.New()}
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

	// The record carries ThreadRecordTTL; a second save keeps CreatedAt.
	e, _, _ := f.Routes.Get(ctx, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T1"})
	require.Equal(t, ThreadRecordTTL, e.TTL)
	created := e.CreatedAt
	require.NoError(t, f.SaveThreadRecord(ctx, "slack", "C1", "T1", store.Thread{AgentRef: "sre-agent", Initiator: "U1", Granted: []string{"U2"}}))
	e, _, _ = f.Routes.Get(ctx, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T1"})
	require.Equal(t, created, e.CreatedAt)
	require.Equal(t, []string{"U2"}, e.Thread.Granted)

	// FindThreadBinding sees an instance binding of the thread under any agent.
	_, ok, err = f.FindThreadBinding(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, f.Routes.Put(ctx, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T1", Agent: "issue-agent"},
		store.Entry{AgentInstanceID: "i-1", CreatedAt: time.Now(), LastSeen: time.Now()}))
	ref, ok, err := f.FindThreadBinding(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "issue-agent", ref)

	// A binding of another thread or channel is not this thread's.
	require.NoError(t, f.Routes.Put(ctx, store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "T2", Agent: "other"},
		store.Entry{AgentInstanceID: "i-2", CreatedAt: time.Now(), LastSeen: time.Now()}))
	ref, ok, err = f.FindThreadBinding(ctx, "slack", "C1", "T1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "issue-agent", ref)
}

func TestThreadRecord_NoStore(t *testing.T) {
	f := &Facade{}
	_, _, err := f.ThreadRecord(context.Background(), "slack", "C1", "T1")
	require.Error(t, err)
	var nilFacade *Facade
	_, _, err = nilFacade.FindThreadBinding(context.Background(), "slack", "C1", "T1")
	require.Error(t, err)
}
