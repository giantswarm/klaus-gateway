package static_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/static"
)

func TestNew_ParsesEntries(t *testing.T) {
	m, err := static.New("test-instance=http://klaus:8080, demo=http://demo:9000 ")
	require.NoError(t, err)

	got, err := m.Get(context.Background(), "test-instance")
	require.NoError(t, err)
	require.Equal(t, "http://klaus:8080", got.BaseURL)

	got, err = m.Get(context.Background(), "demo")
	require.NoError(t, err)
	require.Equal(t, "http://demo:9000", got.BaseURL)
}

func TestNew_EmptySpec(t *testing.T) {
	m, err := static.New("")
	require.NoError(t, err)
	refs, err := m.List(context.Background())
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestNew_InvalidEntry(t *testing.T) {
	_, err := static.New("bad-entry-no-equals")
	require.Error(t, err)
}

func TestGet_NotFound(t *testing.T) {
	m, _ := static.New("a=http://a")
	_, err := m.Get(context.Background(), "missing")
	require.ErrorIs(t, err, lifecycle.ErrNotFound)
}

func TestCreate_ReturnsExisting(t *testing.T) {
	m, _ := static.New("a=http://a")
	ref, err := m.Create(context.Background(), lifecycle.CreateSpec{Name: "a"})
	require.NoError(t, err)
	require.Equal(t, "http://a", ref.BaseURL)
}

func TestCreate_SingleInstanceFallsBack(t *testing.T) {
	m, _ := static.New("only=http://only")
	ref, err := m.Create(context.Background(), lifecycle.CreateSpec{Name: "unknown"})
	require.NoError(t, err)
	require.Equal(t, "only", ref.Name)
}

func TestCreate_MultiInstanceRefusesUnknown(t *testing.T) {
	m, _ := static.New("a=http://a,b=http://b")
	_, err := m.Create(context.Background(), lifecycle.CreateSpec{Name: "unknown"})
	require.Error(t, err)
}
