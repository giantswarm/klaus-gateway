package slack

import (
	"context"
)

// The controller decides whether anyone but a Session's creator may instruct
// its agent: it refuses to share a Session whose Harness may run code a turn
// leaves behind. A collaborator's turn meets that refusal when the facade
// asks for the thread's share, and the turn is not passed on (failureNote
// shows the controller's reason). The thread's row then records it, so the
// access gate refuses later newcomers at once instead of asking the initiator
// to let them in.

// refuseNewcomer refuses, at the access gate, a sender the thread does not
// admit yet when the controller already refused to share the thread's
// Session, with the controller's reason. A row that cannot be read refuses
// nothing here; the turn meets the refusal again once it is admitted.
func (a *Adapter) refuseNewcomer(ctx context.Context, slackChannel, threadID, slackUser string) bool {
	e, ok, err := a.gw.ThreadRecord(ctx, ChannelName, slackChannel, threadID)
	if err != nil || !ok {
		return false
	}
	refusal := e.ShareRefusal
	if refusal == nil || refusal.InstanceID != e.AgentInstanceID {
		return false
	}
	a.Logger.Info("slack: newcomer refused, the controller refuses to share the thread's session",
		"agent", e.AgentRef, "thread", threadID, "slack_user", slackUser)
	a.postRefusal(ctx, slackChannel, slackUser, threadID, refusal.Reason)
	return true
}

func (a *Adapter) postRefusal(ctx context.Context, slackChannel, slackUser, threadID, note string) {
	if err := a.apiClient().postEphemeralText(ctx, slackChannel, slackUser, threadID, note); err != nil {
		a.Logger.Warn("slack: post collaborator refusal failed", "user", slackUser, "error", err)
	}
}
