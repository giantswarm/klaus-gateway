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
	k := store.Key{Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1"}
	require.NoError(t, s1.Put(ctx, k, store.Entry{
		Instance: "inst-42", CreatedAt: time.Now(), LastSeen: time.Now(), TTL: time.Hour,
	}))
	require.NoError(t, s1.Close())

	s2, err := boltstore.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	got, ok, err := s2.Get(ctx, k)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "inst-42", got.Instance)
}
