package channels

import (
	"context"
	"errors"
)

// ErrGroupNotFound is returned by IsGroupMember for a handle no enabled group
// of the workspace carries.
var ErrGroupNotFound = errors.New("group not found")

// GroupMembers answers whether a person belongs to one of the workspace's
// groups (a Slack user group, @team-bumblebee), named by its handle without
// the @: a service that lets a team decide asks it who is on the team.
type GroupMembers interface {
	// IsGroupMember reports whether the workspace member with email is in
	// the group; a person the workspace does not know is not.
	// ErrGroupNotFound when no enabled group has the handle.
	IsGroupMember(ctx context.Context, group, email string) (bool, error)
}
