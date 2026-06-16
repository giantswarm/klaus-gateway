package a2a_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

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
}
