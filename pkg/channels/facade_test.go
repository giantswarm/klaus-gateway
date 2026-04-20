package channels_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/instance"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

type fakeLifecycle struct {
	instances map[string]lifecycle.InstanceRef
}

func (f *fakeLifecycle) Get(_ context.Context, name string) (lifecycle.InstanceRef, error) {
	if ref, ok := f.instances[name]; ok {
		return ref, nil
	}
	return lifecycle.InstanceRef{}, lifecycle.ErrNotFound
}
func (f *fakeLifecycle) Create(_ context.Context, s lifecycle.CreateSpec) (lifecycle.InstanceRef, error) {
	ref := lifecycle.InstanceRef{Name: s.Name, BaseURL: "http://" + s.Name, Status: "ready"}
	if f.instances == nil {
		f.instances = map[string]lifecycle.InstanceRef{}
	}
	f.instances[s.Name] = ref
	return ref, nil
}
func (f *fakeLifecycle) List(context.Context) ([]lifecycle.InstanceRef, error) { return nil, nil }
func (f *fakeLifecycle) Stop(context.Context, string) error                    { return nil }

type fakeClient struct {
	sseBody  string
	messages []instance.Message
	err      error
}

func (f *fakeClient) StreamCompletion(context.Context, channels.InstanceRef, []byte) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.sseBody)), nil
}
func (f *fakeClient) Messages(context.Context, channels.InstanceRef, string) (instance.MessagesResponse, error) {
	return instance.MessagesResponse{Messages: f.messages}, nil
}

func TestFacade_ResolveCreatesInstance(t *testing.T) {
	s := memory.New()
	lm := &fakeLifecycle{}
	router := routing.New(s, lm, true, time.Hour)
	f := &channels.Facade{Router: router}

	ref, err := f.Resolve(context.Background(), channels.InboundMessage{
		Channel:   "web",
		ChannelID: "c1",
		UserID:    "u1",
		ThreadID:  "t1",
	})
	require.NoError(t, err)
	require.NotEmpty(t, ref.Name)

	// And the store now has the mapping.
	entries, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, ref.Name, entries[0].Entry.Instance)
}

func TestFacade_ResolveAutoCreateOffReturnsRouteNotFound(t *testing.T) {
	s := memory.New()
	lm := &fakeLifecycle{}
	router := routing.New(s, lm, false, time.Hour)
	f := &channels.Facade{Router: router}

	_, err := f.Resolve(context.Background(), channels.InboundMessage{
		Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1",
	})
	require.ErrorIs(t, err, routing.ErrRouteNotFound)
}

func TestFacade_SendCompletionEmitsDeltas(t *testing.T) {
	client := &fakeClient{sseBody: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
		"data: [DONE]\n\n"}
	f := &channels.Facade{Client: client}

	ch, err := f.SendCompletion(context.Background(), channels.InstanceRef{Name: "i1"}, channels.InboundMessage{Text: "hi"})
	require.NoError(t, err)

	var parts []string
	var done bool
	for d := range ch {
		if d.Err != nil {
			t.Fatalf("unexpected error: %v", d.Err)
		}
		if d.Done {
			done = true
			continue
		}
		if d.Content != "" {
			parts = append(parts, d.Content)
		}
	}
	require.True(t, done, "expected terminal Done delta")
	require.Equal(t, "hello world", strings.Join(parts, ""))
}

func TestFacade_SendCompletionSurfacesUpstreamError(t *testing.T) {
	client := &fakeClient{err: errors.New("upstream boom")}
	f := &channels.Facade{Client: client}

	_, err := f.SendCompletion(context.Background(), channels.InstanceRef{Name: "i1"}, channels.InboundMessage{Text: "hi"})
	require.Error(t, err)
}

func TestFacade_FetchHistory(t *testing.T) {
	client := &fakeClient{messages: []instance.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}}
	f := &channels.Facade{Client: client}

	msgs, err := f.FetchHistory(context.Background(), channels.InstanceRef{Name: "i1"})
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "user", msgs[0].Role)
	require.Equal(t, "hello", msgs[1].Content)
}

// smoke test that the compile-time interface assertions hold.
var _ store.Store = memory.New()
