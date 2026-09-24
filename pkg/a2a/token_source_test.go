package a2a_test

import (
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

func TestForwardedTokenSource(t *testing.T) {
	t.Run("returns the forwarded token", func(t *testing.T) {
		token, err := pkga2a.ForwardedTokenSource{}.Token(pkga2a.WithForwardedToken(t.Context(), "user-jwt"))
		require.NoError(t, err)
		require.Equal(t, "user-jwt", token)
	})

	t.Run("no forwarded token yields an empty token", func(t *testing.T) {
		token, err := pkga2a.ForwardedTokenSource{ForwardedOnlyChannels: []string{"slack"}}.Token(t.Context())
		require.NoError(t, err)
		require.Empty(t, token)
	})

	t.Run("forwarded-only channel without token errors", func(t *testing.T) {
		src := pkga2a.ForwardedTokenSource{ForwardedOnlyChannels: []string{"slack"}}
		_, err := src.Token(pkga2a.WithChannel(t.Context(), "slack"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "slack")
	})

	t.Run("forwarded-only channel with token succeeds", func(t *testing.T) {
		src := pkga2a.ForwardedTokenSource{ForwardedOnlyChannels: []string{"slack"}}
		ctx := pkga2a.WithForwardedToken(pkga2a.WithChannel(t.Context(), "slack"), "user-jwt")
		token, err := src.Token(ctx)
		require.NoError(t, err)
		require.Equal(t, "user-jwt", token)
	})
}

func TestChannelContext_RoundTrip(t *testing.T) {
	ctx := pkga2a.WithChannel(t.Context(), "slack")
	require.Equal(t, "slack", pkga2a.ChannelFromContext(ctx))
	require.Empty(t, pkga2a.ChannelFromContext(t.Context()))
}
