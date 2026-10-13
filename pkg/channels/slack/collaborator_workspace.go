package slack

import (
	"context"
	"slices"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// A thread's Session in a workspace works on clones of repositories its
// initiator was allowed to read when they chose it. A collaborator's turn runs
// on that Session through its share, so the agent would show them those
// repositories whether they may read them or not: their first turn asks the
// workspace-manager, as them, whether they may (authorize_workspace, the check
// the initiator's choice passed). A pass is remembered in the thread's row
// (store.Entry.WorkspaceReaders), a name and never the grant, for as long as
// the thread keeps that workspace; anything else is answered to them alone and
// asked again on their next turn.

// refuseCollaboratorWorkspace refuses slackUser's turn when the thread's
// Session works in a workspace the workspace-manager does not find them able
// to read, and tells them why. Reports whether the turn was refused. The
// initiator, whose choice was authorized, a turn that makes its own workspace
// choice, a turn for another agent and a thread without a workspace are not
// asked.
func (a *Adapter) refuseCollaboratorWorkspace(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser string) bool {
	if !a.Workspaces.Enabled() || a.gw == nil || msg.Workspace != nil {
		return false
	}
	row, ok, err := a.gw.ThreadRecord(ctx, ChannelName, slackChannel, msg.ThreadID)
	if err != nil {
		// A row that cannot be read is no proof of access.
		a.Logger.Warn("slack: read thread record failed, collaborator turn not passed on", "thread", msg.ThreadID, "user", slackUser, "error", err)
		a.postRefusal(ctx, slackChannel, slackUser, msg.ThreadID, storeUnavailableNotice)
		return true
	}
	if !ok || !row.Workspace.Chosen() || row.AgentRef != msg.AgentRef || row.Initiator == "" || slackUser == row.Initiator ||
		slices.Contains(row.WorkspaceReaders, slackUser) {
		return false
	}

	workspace := row.Workspace.Name
	grant, denial := a.authorizeWorkspace(ctx, slackUser, workspace, collaboratorWorkspaceNotes)
	if !grant.IsZero() {
		a.rememberWorkspaceReader(ctx, slackChannel, msg.ThreadID, slackUser, row.Workspace)
		return false
	}
	a.Logger.Info("slack: collaborator turn refused on the thread's workspace", "record", "workspace_collaborator_refused", "thread", msg.ThreadID, "user", slackUser, "workspace", workspace)
	if len(denial.connect) == 0 {
		a.postRefusal(ctx, slackChannel, slackUser, msg.ThreadID, denial.text)
		return true
	}
	a.postWorkspaceConnectPrompts(ctx, slackChannel, msg.ThreadID, slackUser, workspace, denial.connect, collaboratorWorkspaceConnectTail)
	return true
}

// rememberWorkspaceReader records slackUser as able to read the thread's
// workspace, unless the thread changed it meanwhile.
func (a *Adapter) rememberWorkspaceReader(ctx context.Context, slackChannel, threadID, slackUser string, checked *store.WorkspaceChoice) {
	err := a.gw.UpdateThreadRecord(ctx, ChannelName, slackChannel, threadID, func(e *store.Entry, found bool) bool {
		if !found || !e.Workspace.Equal(checked) || slices.Contains(e.WorkspaceReaders, slackUser) {
			return false
		}
		e.WorkspaceReaders = append(e.WorkspaceReaders, slackUser)
		return true
	})
	if err != nil {
		// Their next turn asks again.
		a.Logger.Warn("slack: remember workspace reader failed", "thread", threadID, "user", slackUser, "error", err)
	}
}
