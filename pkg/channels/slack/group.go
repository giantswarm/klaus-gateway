package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// IsGroupMember reports whether the workspace member with email is in the
// enabled user group whose handle is group. It implements
// channels.GroupMembers. Needs usergroups:read and users:read.email.
func (a *Adapter) IsGroupMember(ctx context.Context, group, email string) (bool, error) {
	client := a.apiClient()
	users, err := client.userGroupUsers(ctx, group)
	if err != nil {
		return false, err
	}
	user, err := client.lookupUserByEmail(ctx, email)
	switch {
	case errors.Is(err, channels.ErrAddresseeNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return slices.Contains(users, user), nil
}

// userGroupUsers returns the user IDs of the enabled user group whose handle
// is handle; no such group is channels.ErrGroupNotFound.
func (c *slackAPIClient) userGroupUsers(ctx context.Context, handle string) ([]string, error) {
	params := url.Values{"include_users": {"true"}}
	body, err := c.call(ctx, "usergroups.list", "application/x-www-form-urlencoded", params.Encode())
	if err != nil {
		return nil, err
	}
	var result struct {
		OK         bool   `json:"ok"`
		Err        string `json:"error,omitempty"`
		Usergroups []struct {
			Handle string   `json:"handle"`
			Users  []string `json:"users"`
		} `json:"usergroups"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("slack usergroups.list: decode: %w", err)
	}
	if !result.OK {
		return nil, &apiError{method: "usergroups.list", code: result.Err}
	}
	for _, g := range result.Usergroups {
		if g.Handle == handle {
			return g.Users, nil
		}
	}
	return nil, channels.ErrGroupNotFound
}
