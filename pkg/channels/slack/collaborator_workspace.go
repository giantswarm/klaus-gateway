package slack

import (
	"context"
	"slices"
	"time"

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
// asked again on their next turn. A provider they connect first holds their
// messages (threadState.workspaceConnect, in the thread's row across a
// restart) for pendingTTL: their next turn that passes runs them, in order,
// before itself.

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
		return a.runWorkspaceConnectHeld(msg, slackChannel, slackUser)
	}

	workspace := row.Workspace.Name
	grant, denial := a.authorizeWorkspace(ctx, slackUser, workspace, collaboratorWorkspaceNotes)
	if !grant.IsZero() {
		a.rememberWorkspaceReader(ctx, slackChannel, msg.ThreadID, slackUser, row.Workspace)
		return a.runWorkspaceConnectHeld(msg, slackChannel, slackUser)
	}
	a.Logger.Info("slack: collaborator turn refused on the thread's workspace", "record", "workspace_collaborator_refused", "thread", msg.ThreadID, "user", slackUser, "workspace", workspace)
	if len(denial.connect) == 0 {
		text := denial.text
		if len(a.takeWorkspaceConnectHeld(msg.ThreadID, slackUser)) > 0 {
			text += collaboratorWorkspaceHeldDroppedTail
		}
		a.postRefusal(ctx, slackChannel, slackUser, msg.ThreadID, text)
		return true
	}
	if a.holdForWorkspaceConnect(msg, slackChannel, slackUser) {
		a.notifyParkedDrop(ctx, slackChannel, msg.ThreadID, slackUser)
	}
	a.postWorkspaceConnectPrompts(ctx, slackChannel, msg.ThreadID, slackUser, workspace, denial.connect, collaboratorWorkspaceConnectTail)
	return true
}

// holdForWorkspaceConnect holds msg of slackUser until they have connected
// the providers the thread's workspace needs. Past maxParkedPerThread their
// oldest held messages go (dropped). Expired holds of every thread are dropped
// on the way.
func (a *Adapter) holdForWorkspaceConnect(msg channels.InboundMessage, slackChannel, slackUser string) (dropped bool) {
	now := time.Now()
	expired := func(m heldMessage) bool { return now.Sub(m.StoredAt) > pendingTTL }
	var swept []store.Key
	a.threadsMu.Lock()
	if a.threads == nil {
		a.threads = make(map[string]*threadState)
	}
	for thread, st := range a.threads {
		for user, held := range st.workspaceConnect {
			slackChannel := held[0].SlackChannel
			if kept := slices.DeleteFunc(held, expired); len(kept) == 0 {
				delete(st.workspaceConnect, user)
				swept = append(swept, store.Key{ChannelID: slackChannel, ThreadID: thread})
			} else {
				st.workspaceConnect[user] = kept
			}
		}
		if st.empty() {
			delete(a.threads, thread)
		}
	}
	st := a.threads[msg.ThreadID]
	if st == nil {
		st = &threadState{}
		a.threads[msg.ThreadID] = st
	}
	if st.workspaceConnect == nil {
		st.workspaceConnect = make(map[string][]heldMessage)
	}
	held := append(st.workspaceConnect[slackUser], heldMessageOf(msg, slackChannel, now))
	if len(held) > maxParkedPerThread {
		held, dropped = lastN(held, maxParkedPerThread), true
	}
	st.workspaceConnect[slackUser] = held
	a.threadsMu.Unlock()

	for _, k := range swept {
		a.persistHeld(k.ChannelID, k.ThreadID)
	}
	a.persistHeld(slackChannel, msg.ThreadID)
	return dropped
}

// takeWorkspaceConnectHeld removes and returns the messages slackUser's
// thread holds for their connect, in arrival order, without the expired ones.
func (a *Adapter) takeWorkspaceConnectHeld(threadID, slackUser string) []heldMessage {
	var held []heldMessage
	a.withThread(threadID, func(st *threadState) {
		held = st.workspaceConnect[slackUser]
		delete(st.workspaceConnect, slackUser)
	})
	if len(held) == 0 {
		return nil
	}
	a.persistHeld(held[0].SlackChannel, threadID)
	now := time.Now()
	return slices.DeleteFunc(held, func(m heldMessage) bool { return now.Sub(m.StoredAt) > pendingTTL })
}

// runWorkspaceConnectHeld runs the messages slackUser's connect held, then
// msg, once their turn passes the workspace check, and reports whether it took
// msg over. The replays wait for the thread's slot, which msg's caller frees
// on return.
func (a *Adapter) runWorkspaceConnectHeld(msg channels.InboundMessage, slackChannel, slackUser string) bool {
	held := a.takeWorkspaceConnectHeld(msg.ThreadID, slackUser)
	if len(held) == 0 {
		return false
	}
	held = append(held, heldMessageOf(msg, slackChannel, time.Now()))
	a.Logger.Info("slack: collaborator's held messages run after connecting", "record", "workspace_collaborator_held_run", "thread", msg.ThreadID, "user", slackUser, "messages", len(held))
	a.background(func(ctx context.Context) {
		for _, m := range held {
			a.replayHeld(ctx, msg.ThreadID, m.Msg, m.SlackChannel)
		}
	})
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
