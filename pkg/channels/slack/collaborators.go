package slack

import (
	"context"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// An agent whose Harness runs code (a claude Harness) takes turns from one
// person only: the one whose Session the thread is bound to. Its workspace
// carries code as well as data across turns — a git hook, a Makefile target,
// a background process — and every turn's processes run as the same user, so
// a collaborator's turn would run what the owner's left behind under the
// collaborator's credential, and the other way round. The thread's grants and
// Session share stay for every other agent; for this one a collaborator is
// refused and pointed at a thread of their own.

// collaboratorPolicy reports whether an agent takes turns from the owner of
// its Session only. pkg/a2a.Client implements it, from the agent's Harness.
type collaboratorPolicy interface {
	RefusesCollaborators(ctx context.Context, agentRef string) (bool, error)
}

// refusesCollaborators reports whether agentRef's agent takes turns from the
// owner of its Session only. A gateway whose roster has no such policy (no
// kagent client) serves no agent that does.
func (a *Adapter) refusesCollaborators(ctx context.Context, agentRef string) (bool, error) {
	policy, ok := a.Roster.(collaboratorPolicy)
	if !ok || agentRef == "" {
		return false, nil
	}
	return policy.RefusesCollaborators(ctx, agentRef)
}

// refuseCollaborator refuses a turn by anyone but the owner of the Session the
// thread is bound to, on an agent that refuses collaborators, and tells the
// sender why. An agent that cannot be looked up refuses too: whether it shares
// its Session is unknown. Reports whether the turn was refused. The access
// gate already let the sender in, so this is the check every entrypoint's turn
// passes, typed or clicked.
func (a *Adapter) refuseCollaborator(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser string) bool {
	owner := a.accessPolicy().SessionOwner(ctx, msg.ChannelID, msg.ThreadID, msg.AgentRef)
	if owner == "" || owner == slackUser {
		return false
	}
	refuses, err := a.refusesCollaborators(pkga2a.WithForwardedToken(ctx, msg.BearerToken), msg.AgentRef)
	note := collaboratorRefusedNote
	switch {
	case err != nil:
		a.Logger.Warn("slack: agent lookup failed, collaborator turn not passed on",
			"agent", msg.AgentRef, "thread", msg.ThreadID, "slack_user", slackUser, "error", err)
		note = collaboratorCheckFailedNote
	case !refuses:
		return false
	default:
		a.Logger.Info("slack: collaborator turn refused, the agent takes its owner's turns only",
			"agent", msg.AgentRef, "thread", msg.ThreadID, "slack_user", slackUser, "owner", owner)
	}
	a.postRefusal(ctx, slackChannel, slackUser, msg.ThreadID, note)
	return true
}

// refuseNewcomer refuses, at the access gate, a sender the thread does not
// admit yet when its agent refuses collaborators: asking the initiator to let
// them in would only lead to a refused turn. A failed lookup refuses nothing
// here; the turn's own check (refuseCollaborator) decides once it is admitted.
func (a *Adapter) refuseNewcomer(ctx context.Context, slackChannel, threadID, slackUser string) bool {
	agentRef := a.boundAgentOrDefault(ctx, slackChannel, threadID)
	refuses, err := a.refusesCollaborators(a.withCallerToken(ctx, slackUser), agentRef)
	if err != nil || !refuses {
		return false
	}
	a.Logger.Info("slack: newcomer refused, the agent takes its owner's turns only",
		"agent", agentRef, "thread", threadID, "slack_user", slackUser)
	a.postRefusal(ctx, slackChannel, slackUser, threadID, collaboratorRefusedNote)
	return true
}

func (a *Adapter) postRefusal(ctx context.Context, slackChannel, slackUser, threadID, note string) {
	if err := a.apiClient().postEphemeralText(ctx, slackChannel, slackUser, threadID, note); err != nil {
		a.Logger.Warn("slack: post collaborator refusal failed", "user", slackUser, "error", err)
	}
}
