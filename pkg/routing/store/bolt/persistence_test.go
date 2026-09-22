package bolt_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	boltstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/bolt"
)

// TestPersistence verifies the acceptance-criteria guarantee that the routing
// table survives a process restart in bolt mode.
func TestPersistence(t *testing.T) {
	path := t.TempDir() + "/routes.bolt"
	ctx := context.Background()

	s1, err := boltstore.Open(path)
	require.NoError(t, err)
	k := store.Key{Channel: "slack", ChannelID: "c1", ThreadID: "t1"}
	require.NoError(t, s1.Put(ctx, k, store.Entry{
		AgentInstanceID: "inst-42", CreatedAt: time.Now(), LastSeen: time.Now(), TTL: time.Hour,
	}))
	require.NoError(t, s1.Close())

	s2, err := boltstore.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	got, ok, err := s2.Get(ctx, k)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "inst-42", got.AgentInstanceID)
}

// A thread's kagent binding carries the task in flight and the channel's
// resume data across a restart: that record is what lets the next process
// deliver the turn's result.
func TestPersistence_InFlightTask(t *testing.T) {
	path := t.TempDir() + "/routes.bolt"
	ctx := context.Background()

	s1, err := boltstore.Open(path)
	require.NoError(t, err)
	k := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001"}
	require.NoError(t, s1.Put(ctx, k, store.Entry{
		AgentRef: "sre", AgentInstanceID: "inst-1", TaskID: "task-7",
		Resume:    map[string]string{"slack_user": "U1", "message_ts": "1700.0001"},
		CreatedAt: time.Now(), LastSeen: time.Now(),
	}))
	require.NoError(t, s1.Close())

	s2, err := boltstore.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	got, ok, err := s2.Get(ctx, k)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "sre", got.AgentRef)
	require.Equal(t, "inst-1", got.AgentInstanceID)
	require.Equal(t, "task-7", got.TaskID)
	require.Equal(t, map[string]string{"slack_user": "U1", "message_ts": "1700.0001"}, got.Resume)
}
