package routing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

type stubLifecycle struct {
	getFn    func(ctx context.Context, name string) (lifecycle.InstanceRef, error)
	createFn func(ctx context.Context, spec lifecycle.CreateSpec) (lifecycle.InstanceRef, error)
}

func (s *stubLifecycle) Get(ctx context.Context, name string) (lifecycle.InstanceRef, error) {
	if s.getFn != nil {
		return s.getFn(ctx, name)
	}
	return lifecycle.InstanceRef{}, lifecycle.ErrNotFound
}

func (s *stubLifecycle) Create(ctx context.Context, spec lifecycle.CreateSpec) (lifecycle.InstanceRef, error) {
	if s.createFn != nil {
		return s.createFn(ctx, spec)
	}
	return lifecycle.InstanceRef{}, errors.New("create not stubbed")
}

func (s *stubLifecycle) List(context.Context) ([]lifecycle.InstanceRef, error) {
	return nil, nil
}

func (s *stubLifecycle) Stop(context.Context, string) error { return nil }

func TestRouter_CacheHit(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	t.Cleanup(func() { _ = s.Close() })

	k := store.Key{Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1"}
	require.NoError(t, s.Put(ctx, k, store.Entry{Instance: "i1", LastSeen: time.Now(), TTL: time.Hour}))

	mgr := &stubLifecycle{
		getFn: func(_ context.Context, name string) (lifecycle.InstanceRef, error) {
			require.Equal(t, "i1", name)
			return lifecycle.InstanceRef{Name: name, BaseURL: "http://i1"}, nil
		},
		createFn: func(context.Context, lifecycle.CreateSpec) (lifecycle.InstanceRef, error) {
			t.Fatal("create must not be called on cache hit")
			return lifecycle.InstanceRef{}, nil
		},
	}
	r := routing.New(s, mgr, false, time.Hour)
	ref, err := r.Resolve(ctx, routing.InboundMessage{Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1"})
	require.NoError(t, err)
	require.Equal(t, "i1", ref.Name)
}

func TestRouter_CacheMiss_AutoCreate(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	t.Cleanup(func() { _ = s.Close() })

	created := false
	mgr := &stubLifecycle{
		createFn: func(_ context.Context, spec lifecycle.CreateSpec) (lifecycle.InstanceRef, error) {
			created = true
			return lifecycle.InstanceRef{Name: spec.Name, BaseURL: "http://new"}, nil
		},
	}
	r := routing.New(s, mgr, true, time.Hour)
	ref, err := r.Resolve(ctx, routing.InboundMessage{
		Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1", NameHint: "klaus-abc",
	})
	require.NoError(t, err)
	require.Equal(t, "klaus-abc", ref.Name)
	require.True(t, created)

	// Mapping should be persisted.
	got, ok, err := s.Get(ctx, store.Key{Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "klaus-abc", got.Instance)
}

func TestRouter_CacheMiss_NoAutoCreate(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	t.Cleanup(func() { _ = s.Close() })

	mgr := &stubLifecycle{}
	r := routing.New(s, mgr, false, time.Hour)
	_, err := r.Resolve(ctx, routing.InboundMessage{Channel: "web", ChannelID: "c1"})
	require.ErrorIs(t, err, routing.ErrRouteNotFound)
}
