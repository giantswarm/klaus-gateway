package slack

import (
	"context"
	"fmt"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// turnTail carries the per-entrypoint pieces of runTurn, the shared tail of a
// Slack turn.
type turnTail struct {
	// attach binds the pending input-required task this turn resumes to msg,
	// returning it (nil = a fresh turn). It runs after the sender's identity
	// is resolved, so it may consult the session under the turn's real
	// identity. runTurn owns re-storing the returned task when a later
	// failure would otherwise strand it.
	attach func(msg *channels.InboundMessage) *pendingTask
	// onResolved runs once the agent resolved, before the completion is sent
	// (e.g. the launch announcement). nil = nothing.
	onResolved func(msg channels.InboundMessage)
	// failureNote posts the user-visible note when resolve or send fails:
	// the turn dies before any streamed reply, so silence reads as success.
	failureNote func()
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
// the raw Slack user ID (slackUser) and BearerToken set to the sender's
// human token.
func (a *Adapter) runTurn(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser string, tail turnTail) error {
	a.resolveSubjectEmail(ctx, &msg)

	a.applyInitiatorIdentity(ctx, &msg, msg.ThreadID, slackUser)

	task := tail.attach(&msg)
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
		tail.failureNote()
		return fmt.Errorf("slack: resolve: %w", err)
	}

	if tail.onResolved != nil {
		tail.onResolved(msg)
	}

	turnCtx, done := a.registerTurn(ctx, msg.ThreadID)
	defer done()

	a.logTurnDispatch(msg, slackUser, task != nil)

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		restoreTask()
		tail.failureNote()
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
