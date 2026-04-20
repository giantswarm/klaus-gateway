package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/giantswarm/klaus-gateway/pkg/api/v1alpha1"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	boltstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/bolt"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/configmap"
	crdstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/crd"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// runConformance exercises the Store contract. Every backend must pass.
func runConformance(t *testing.T, factory func(t *testing.T) store.Store) {
	t.Helper()

	t.Run("put-get-delete", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1"}
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
			{Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1"},
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
		k := store.Key{Channel: "web", ChannelID: "c|pipe", UserID: "u1", ThreadID: `t\back`}
		require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "inst", LastSeen: time.Now(), TTL: time.Hour}))
		got, ok, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "inst", got.Instance)
	})

	t.Run("ttl-expired-filtered", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		k := store.Key{Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1"}
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

func TestConfigMapStore_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) store.Store {
		client := fake.NewSimpleClientset()
		s := configmap.New(client, configmap.Options{Namespace: "default"})
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestCRDStore_Conformance(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	runConformance(t, func(t *testing.T) store.Store {
		fakeClient := ctrlfake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&v1alpha1.ChannelRoute{}).
			Build()
		s := crdstore.New(fakeClient, "default")
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestKey_StringRoundTrip(t *testing.T) {
	cases := []store.Key{
		{Channel: "web", ChannelID: "abc", UserID: "u1", ThreadID: "t1"},
		{Channel: "slack", ChannelID: "C|123", UserID: `user\1`, ThreadID: ""},
	}
	for _, k := range cases {
		parsed, err := store.ParseKey(k.String())
		require.NoError(t, err)
		require.Equal(t, k, parsed)
	}
}
