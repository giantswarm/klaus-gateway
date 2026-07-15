package a2a_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

type errorTokenSource struct{ err error }

func (s errorTokenSource) Token(_ context.Context) (string, error) { return "", s.err }

func TestForwardedTokenContext_RoundTrip(t *testing.T) {
	ctx := pkga2a.WithForwardedToken(t.Context(), "user-jwt")
	require.Equal(t, "user-jwt", pkga2a.ForwardedTokenFromContext(ctx))
}

func TestWithForwardedToken_EmptyLeavesContextUnchanged(t *testing.T) {
	ctx := pkga2a.WithForwardedToken(t.Context(), "")
	require.Empty(t, pkga2a.ForwardedTokenFromContext(ctx))
}

func TestFileTokenSource(t *testing.T) {
	t.Run("reads and trims file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte("  sa-token\n"), 0o600))

		token, err := pkga2a.FileTokenSource{Path: path}.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, "sa-token", token)
	})

	t.Run("empty path yields empty token", func(t *testing.T) {
		token, err := pkga2a.FileTokenSource{}.Token(t.Context())
		require.NoError(t, err)
		require.Empty(t, token)
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, err := pkga2a.FileTokenSource{Path: filepath.Join(t.TempDir(), "absent")}.Token(t.Context())
		require.Error(t, err)
	})
}

func TestForwardedTokenSource(t *testing.T) {
	t.Run("prefers forwarded token", func(t *testing.T) {
		src := pkga2a.ForwardedTokenSource{Fallback: pkga2a.FileTokenSource{Path: "/should/not/be/read"}}
		ctx := pkga2a.WithForwardedToken(t.Context(), "user-jwt")

		token, err := src.Token(ctx)
		require.NoError(t, err)
		require.Equal(t, "user-jwt", token)
	})

	t.Run("falls back when no forwarded token", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte("sa-token"), 0o600))
		src := pkga2a.ForwardedTokenSource{Fallback: pkga2a.FileTokenSource{Path: path}}

		token, err := src.Token(t.Context())
		require.NoError(t, err)
		require.Equal(t, "sa-token", token)
	})

	t.Run("nil fallback yields empty token", func(t *testing.T) {
		token, err := pkga2a.ForwardedTokenSource{}.Token(t.Context())
		require.NoError(t, err)
		require.Empty(t, token)
	})

	t.Run("fallback error is propagated", func(t *testing.T) {
		src := pkga2a.ForwardedTokenSource{Fallback: errorTokenSource{err: errors.New("vault unavailable")}}

		_, err := src.Token(t.Context())
		require.EqualError(t, err, "vault unavailable")
	})

	t.Run("forwarded-only channel without token errors", func(t *testing.T) {
		src := pkga2a.ForwardedTokenSource{
			Fallback:              pkga2a.FileTokenSource{Path: "/should/not/be/read"},
			ForwardedOnlyChannels: []string{"slack"},
		}
		ctx := pkga2a.WithChannel(t.Context(), "slack")

		_, err := src.Token(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "slack")
	})

	t.Run("forwarded-only channel with token succeeds", func(t *testing.T) {
		src := pkga2a.ForwardedTokenSource{ForwardedOnlyChannels: []string{"slack"}}
		ctx := pkga2a.WithChannel(t.Context(), "slack")
		ctx = pkga2a.WithForwardedToken(ctx, "user-jwt")

		token, err := src.Token(ctx)
		require.NoError(t, err)
		require.Equal(t, "user-jwt", token)
	})

	t.Run("other channel keeps the fallback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte("sa-token"), 0o600))
		src := pkga2a.ForwardedTokenSource{
			Fallback:              pkga2a.FileTokenSource{Path: path},
			ForwardedOnlyChannels: []string{"slack"},
		}
		ctx := pkga2a.WithChannel(t.Context(), "web")

		token, err := src.Token(ctx)
		require.NoError(t, err)
		require.Equal(t, "sa-token", token)
	})
}

func TestChannelContext_RoundTrip(t *testing.T) {
	ctx := pkga2a.WithChannel(t.Context(), "slack")
	require.Equal(t, "slack", pkga2a.ChannelFromContext(ctx))
	require.Empty(t, pkga2a.ChannelFromContext(t.Context()))
}
