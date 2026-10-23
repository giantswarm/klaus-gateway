package slack_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestIsGroupMember(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})
	fake.setResponder("usergroups.list", func(p map[string]any) string {
		require.Equal(t, "true", p["include_users"])
		return `{"ok":true,"usergroups":[{"handle":"team-planeteers","users":["U2"]},{"handle":"team-bumblebee","users":["U1","U3"]}]}`
	})
	fake.setResponder("users.lookupByEmail", func(p map[string]any) string {
		switch p["email"] {
		case "u1@example.com":
			return `{"ok":true,"user":{"id":"U1"}}`
		case "u2@example.com":
			return `{"ok":true,"user":{"id":"U2"}}`
		}
		return `{"ok":false,"error":"users_not_found"}`
	})
	ctx := context.Background()

	member, err := a.IsGroupMember(ctx, "team-bumblebee", "u1@example.com")
	require.NoError(t, err)
	require.True(t, member)

	member, err = a.IsGroupMember(ctx, "team-bumblebee", "u2@example.com")
	require.NoError(t, err)
	require.False(t, member, "another group's member")

	member, err = a.IsGroupMember(ctx, "team-bumblebee", "stranger@example.com")
	require.NoError(t, err)
	require.False(t, member, "nobody in the workspace")

	_, err = a.IsGroupMember(ctx, "team-nobody", "u1@example.com")
	require.ErrorIs(t, err, channels.ErrGroupNotFound)
}

func TestIsGroupMember_MissingScopeIsAnError(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})
	fake.setResponder("usergroups.list", func(map[string]any) string { return `{"ok":false,"error":"missing_scope"}` })

	_, err := a.IsGroupMember(context.Background(), "team-bumblebee", "u1@example.com")
	require.ErrorContains(t, err, "missing_scope")
}
