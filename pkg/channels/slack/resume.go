package slack

import (
	"context"
	"errors"
	"fmt"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// turnResumer is the optional Gateway capability behind restart recovery: the
// record of the turns a process left running at the controller, and the
// resubscription that delivers their results. The facade implements it once a
// kagent client and a routing store are wired.
type turnResumer interface {
	// ResumesTurns reports whether a turn cut short by a shutdown is delivered
	// after the restart (the record must outlive the process).
	ResumesTurns() bool
	InFlightTurns(ctx context.Context, channel string) ([]channels.InFlightTurn, error)
	InFlightTurn(ctx context.Context, msg channels.InboundMessage) (channels.InFlightTurn, bool, error)
	ResumeTurn(ctx context.Context, msg channels.InboundMessage, taskID string) (<-chan channels.OutboundDelta, error)
}

// Keys of the resume data a turn is dispatched with (InboundMessage.Resume);
// the thread's Slack channel is the routing key's ChannelID already.
const (
	resumeKeyUser    = "slack_user" // the Slack user whose identity the turn runs under
	resumeKeyMessage = "message_ts" // the triggering message the progress reaction sits on
)

// resumeData is what a later process needs to deliver the turn's result into
// its thread: whose token to mint and which message to react on. triggerTS is
// empty for a button resume, which then renders text progress.
func resumeData(slackUser, triggerTS string) map[string]string {
	data := map[string]string{resumeKeyUser: slackUser}
	if triggerTS != "" {
		data[resumeKeyMessage] = triggerTS
	}
	return data
}

// restartedNotice tells a thread that the gateway restarted while the agent
// was working on it. The wording promises a delivery only when the gateway can
// keep that promise: without a routing store that outlives the process, the
// turn is unreachable once this one exits.
const (
	restartedResumesNotice = "⚠️ I was restarted while **%s** was working. It keeps going — the result is in the Dev Portal, and I post it here when it is done."
	restartedNotice        = "⚠️ I was restarted while **%s** was working. It keeps going and its result lands in the Dev Portal, but I cannot bring it into this thread; please ask again if you need it here."
	// resumeLostNote replaces the restart notice's promise when the controller
	// no longer has the task the previous process left running.
	resumeLostNote = "_(I couldn't recover the result of the turn my restart interrupted; please ask again)_"
)

func (a *Adapter) restartedNotice(ctx context.Context, agentRef string) string {
	name := a.agentNameFor(ctx, agentRef)
	if r, ok := a.gw.(turnResumer); ok && r.ResumesTurns() {
		return fmt.Sprintf(restartedResumesNotice, name)
	}
	return fmt.Sprintf(restartedNotice, name)
}

// Recovery of a turn: how many times the start-up pass retries a turn whose
// delivery failed for a reason that may clear (the controller not reachable
// yet, a token mint that timed out), how long it waits between tries, and how
// long the listing of the records may take.
const (
	recoverAttempts    = 3
	recoverRetryDelay  = 10 * time.Second
	recoverListTimeout = 15 * time.Second
	// recoverLookupTimeout bounds the per-thread record lookup a reply makes
	// on the turn's critical path.
	recoverLookupTimeout = 3 * time.Second
)

// recoverOutcome is what one delivery attempt of a left-running turn came to.
type recoverOutcome int

const (
	recoverDone  recoverOutcome = iota // delivered, or nothing left to deliver
	recoverSkip                        // not this process's to deliver now (thread busy, user signed out)
	recoverRetry                       // failed for a reason that may clear
)

// RecoverTurns delivers, in the background, the results of the turns a
// previous process left running at the controller. Call it once the gateway
// is fully wired (kagent client, account linking, roster): a resubscription
// needs all of them. Without the capability nothing happens.
func (a *Adapter) RecoverTurns() {
	if _, ok := a.gw.(turnResumer); ok {
		a.background(a.recoverTurns)
	}
}

func (a *Adapter) recoverTurns(ctx context.Context) {
	resumer := a.gw.(turnResumer)
	lctx, cancel := context.WithTimeout(ctx, recoverListTimeout)
	turns, err := resumer.InFlightTurns(lctx, ChannelName)
	cancel()
	if err != nil {
		a.Logger.Warn("slack: list of turns left running by the previous process unavailable", "error", err)
		return
	}
	if len(turns) == 0 {
		return
	}
	a.Logger.Info("slack: recovering turns left running by the previous process", "record", "turns_recover", "count", len(turns))
	for _, turn := range turns {
		a.background(func(ctx context.Context) { a.recoverTurn(ctx, resumer, turn) })
	}
}

// recoverTurn delivers one left-running turn, retrying a failure that may
// clear. A turn given up on stays recorded: the thread's next reply delivers
// it (dispatch's deliverLeftoverTurn).
func (a *Adapter) recoverTurn(ctx context.Context, resumer turnResumer, turn channels.InFlightTurn) {
	for attempt := 1; ; attempt++ {
		token, outcome := a.recoveryToken(ctx, turn.Msg.Resume[resumeKeyUser], "")
		if outcome == recoverDone {
			if !a.acquireThread(turn.Msg.ThreadID) {
				return // another turn holds the thread; the reply path delivers
			}
			outcome = a.deliverInFlight(ctx, resumer, turn, token)
			a.releaseThread(turn.Msg.ThreadID)
		}
		if outcome != recoverRetry {
			return
		}
		if attempt == recoverAttempts {
			a.Logger.Warn("slack: turn left running by the previous process not recovered; delivered on the thread's next reply",
				"thread", turn.Msg.ThreadID, "task", turn.TaskID)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(recoverRetryDelay):
		}
	}
}

// deliverLeftoverTurn delivers the result of a turn a previous process left
// running on msg's thread, when the record of one exists, before the reply
// that found it is handled. The caller holds the thread's slot; the delivery
// runs under the recorded user's identity when their token can be minted, and
// under the reply's otherwise. Best effort: a failure leaves the record for a
// later reply.
func (a *Adapter) deliverLeftoverTurn(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser string) {
	resumer, ok := a.gw.(turnResumer)
	if !ok {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, recoverLookupTimeout)
	turn, found, err := resumer.InFlightTurn(lctx, msg)
	cancel()
	if err != nil {
		a.Logger.Warn("slack: lookup of a turn left running on the thread failed", "thread", msg.ThreadID, "error", err)
		return
	}
	if !found {
		return
	}
	token, outcome := a.recoveryToken(ctx, turn.Msg.Resume[resumeKeyUser], msg.BearerToken)
	if outcome != recoverDone {
		return
	}
	a.deliverInFlight(ctx, resumer, turn, token)
}

// recoveryToken is the token a left-running turn's delivery runs under: the
// recorded user's, minted afresh; fallback (a reply's own token) when they are
// signed out or the mint fails. Without account linking there is no token to
// mint and fallback is what there is. A signed-out user with no fallback is a
// skip (their next reply carries a token); a failed mint with no fallback may
// clear and is a retry.
func (a *Adapter) recoveryToken(ctx context.Context, slackUser, fallback string) (string, recoverOutcome) {
	if a.OBO == nil {
		return fallback, recoverDone
	}
	if slackUser == "" {
		if fallback == "" {
			return "", recoverSkip
		}
		return fallback, recoverDone
	}
	token, err := a.OBO.TokenFor(ctx, slackUser)
	switch {
	case err == nil:
		return token, recoverDone
	case fallback != "":
		return fallback, recoverDone
	case errors.Is(err, musterlink.ErrNotLinked):
		a.Logger.Info("slack: user of a turn left running is signed out; delivered on their next reply", "user", slackUser)
		return "", recoverSkip
	default:
		a.Logger.Warn("slack: token for a turn left running unavailable", "user", slackUser, "error", err)
		return "", recoverRetry
	}
}

// deliverInFlight resubscribes to turn's task under token and streams what is
// left of it — or its finished answer — into the thread, with the same
// progress rendering as the turn it continues (the working reaction on the
// original message, when the record names one). The caller holds the thread's
// slot. The thread is marked active under the recorded user, so plain replies
// into it are served again after the restart.
func (a *Adapter) deliverInFlight(ctx context.Context, resumer turnResumer, turn channels.InFlightTurn, token string) recoverOutcome {
	slackUser := turn.Msg.Resume[resumeKeyUser]
	triggerTS := turn.Msg.Resume[resumeKeyMessage]
	slackChannel := turn.Msg.ChannelID
	threadID := turn.Msg.ThreadID
	msg := turn.Msg
	msg.Subject = slackUser
	msg.BearerToken = token
	msg.MessageID = triggerTS
	if slackUser != "" {
		a.accessPolicy().SetInitiator(threadID, slackUser)
	}

	turnCtx, done := a.registerTurn(ctx, threadID)
	defer done()

	client := a.agentClient(pkga2a.WithForwardedToken(ctx, token), msg.AgentRef)
	deltas, err := resumer.ResumeTurn(turnCtx, msg, turn.TaskID)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return recoverSkip
		case errors.Is(err, a2apkg.ErrTaskNotFound), pkga2a.IsNotFound(err):
			a.Logger.Warn("slack: turn left running by the previous process is gone at the controller", "thread", threadID, "task", turn.TaskID, "error", err)
			a.postTerminalNote(ctx, client, slackChannel, threadID, "", resumeLostNote)
			return recoverDone
		default:
			a.Logger.Warn("slack: resubscribe to a turn left running failed", "thread", threadID, "task", turn.TaskID, "error", err)
			return recoverRetry
		}
	}
	a.Logger.Info("slack: delivering a turn left running by the previous process",
		"record", "turn_resume", "agent", msg.AgentRef, "slack_user", slackUser,
		"channel_id", msg.ChannelID, "thread_id", threadID, "task_id", turn.TaskID)
	if err := a.streamResponse(turnCtx, client, deltas, msg, slackUser, slackChannel, threadID, triggerTS, thinkingPlaceholder, channels.TurnUsage{}); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Warn("slack: delivery of a turn left running failed", "thread", threadID, "task", turn.TaskID, "error", err)
	}
	return recoverDone
}
