package klausctl_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/klausctl"
)

type fakeRunner struct {
	outputs map[string][]byte
	last    []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.last = args
	key := args[0]
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	return nil, nil
}

func TestKlausctl_Create(t *testing.T) {
	r := &fakeRunner{outputs: map[string][]byte{
		"run": []byte(`{"name":"i1","base_url":"http://host:8080","mcp_url":"http://host:8080/mcp","status":"ready"}`),
	}}
	m, err := klausctl.New("klausctl", klausctl.WithRunner(r))
	require.NoError(t, err)
	ref, err := m.Create(context.Background(), lifecycle.CreateSpec{
		Name: "i1", Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1",
	})
	require.NoError(t, err)
	require.Equal(t, "i1", ref.Name)
	require.Equal(t, "http://host:8080", ref.BaseURL)
	require.Contains(t, r.last, "--channel")
	require.Contains(t, r.last, "web")
}

func TestKlausctl_List(t *testing.T) {
	r := &fakeRunner{outputs: map[string][]byte{
		"list": []byte(`[{"name":"a"},{"name":"b"}]`),
	}}
	m, err := klausctl.New("klausctl", klausctl.WithRunner(r))
	require.NoError(t, err)
	refs, err := m.List(context.Background())
	require.NoError(t, err)
	require.Len(t, refs, 2)
}
