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
