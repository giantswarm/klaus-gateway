package slack

import (
	"context"
	"fmt"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// turnTail carries the per-entrypoint pieces of runTurn, the shared tail of a
// Slack turn. The on* fields are notification hooks (nil = nothing);
// attachTask is a provider, so it does not share the prefix.
type turnTail struct {
	// onIdentityResolved runs once the sender's email is resolved and the
	// turn's forwarded identity is final, before the pending task is
	// attached, so it may probe the session under the identity the turn will
	// run as (e.g. the resume-degradation announcement).
	onIdentityResolved func(msg channels.InboundMessage)
	// attachTask binds the pending input-required task this turn resumes to
	// msg, returning it (nil = a fresh turn). runTurn owns re-storing the
	// returned task when a later failure would otherwise strand it.
	attachTask func(msg *channels.InboundMessage) *pendingTask
	// onAgentResolved runs once the agent resolved, before the completion is
	// sent (e.g. the launch announcement).
	onAgentResolved func(msg channels.InboundMessage)
	// onFailure posts the user-visible note when resolve or send fails: the
	// turn dies before any streamed reply, so silence reads as success. Not
	// called for a corrupt-session failure, where the recovery notice speaks
	// instead (a "retry" invitation would retry into the deleted session).
	onFailure func()
	// triggerTS is the user message the progress reaction lands on; "" uses
	// text progress (a button resume has no user message to react to).
	triggerTS   string
	placeholder string
}

// runTurn is the shared tail of a Slack turn, converged on by dispatch (a
// typed message) and handleDecision (a button click) after their own
// admission: it resolves the sender's email, applies the initiator's identity
// for a shared thread, attaches the pending task, resolves the agent, and
// streams the completion. It owns the restore-on-failure guard: a failure
// after the pending task was taken and before the stream runs re-stores it,
// so a retry or button click can still resume the paused A2A task.
//
// The caller must hold the thread's slot and pass msg with Subject set to
// the raw Slack user ID and BearerToken set to the sender's human token.
func (a *Adapter) runTurn(ctx context.Context, msg channels.InboundMessage, slackChannel string, tail turnTail) (err error) {
	// Subject is rewritten to the resolved email below; the raw ID keys access
	// control and progress surfaces throughout.
	slackUser := msg.Subject

	// Corrupt-session recovery lives here, not in the callers: the reset must
	// present the identity the turn ran under (the initiator's token after the
	// swap below, since kagent keys the session lookup on the token's
	// principal), and only this msg copy carries it. A corrupt session
	// invalidates any pending task inside it, so the handle is dropped rather
	// than left (or re-stored by a failure branch) for a resume that can only
	// fail against the deleted session.
	defer func() {
		if !isCorruptSessionErr(err) {
			return
		}
		a.takePendingTask(msg.ThreadID)
		a.recoverCorruptSession(ctx, msg, slackChannel)
	}()

	a.resolveSubjectEmail(ctx, &msg)

	a.applyInitiatorIdentity(ctx, &msg, msg.ThreadID, slackUser)

	if tail.onIdentityResolved != nil {
		tail.onIdentityResolved(msg)
	}

	var task *pendingTask
	if tail.attachTask != nil {
		task = tail.attachTask(&msg)
	}
	// A failure between the take and a running stream would otherwise strand the
	// paused A2A task (the take deleted the only handle to it); put it back so a
	// retry or button click can still resume it.
	restoreTask := func() {
		if task != nil {
			a.storePendingTask(msg.ThreadID, task)
		}
	}

	ref, err := a.gw.Resolve(ctx, msg)
	if err != nil {
		restoreTask()
		if tail.onFailure != nil && !isCorruptSessionErr(err) {
			tail.onFailure()
		}
		return fmt.Errorf("slack: resolve: %w", err)
	}

	if tail.onAgentResolved != nil {
		tail.onAgentResolved(msg)
	}

	turnCtx, done := a.registerTurn(ctx, msg.ThreadID)
	defer done()

	a.logTurnDispatch(msg, slackUser, task != nil)

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		restoreTask()
		if tail.onFailure != nil && !isCorruptSessionErr(err) {
			tail.onFailure()
		}
		return fmt.Errorf("slack: send completion: %w", err)
	}

	// The turn context feeds the whole stream so /stop cancels the turn, and an
	// aborted consumer releases the producer goroutine.
	var carried channels.TurnUsage
	if task != nil {
		carried = task.Usage
	}
	return a.streamResponse(turnCtx, a.agentClient(ctx, msg.AgentRef), deltas, msg, slackUser, slackChannel, msg.ThreadID, tail.triggerTS, tail.placeholder, carried)
}
