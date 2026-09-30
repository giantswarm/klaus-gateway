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

// Keys of the resume data a turn is dispatched with (InboundMessage.Resume);
// the thread's Slack channel is the routing key's ChannelID already.
const (
	resumeKeyUser    = "slack_user" // the Slack user whose identity the turn runs under
	resumeKeyMessage = "message_ts" // the triggering message the progress reaction sits on
)

// resumeData is what a later process needs to deliver the turn's result into
// its thread: whose token to mint and which message to react on. triggerTS is
// empty for a button resume, which then gets no reaction.
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
	restartedResumesNotice = "The gateway restarted while *%s* was working. The agent keeps going: its result is in the Dev Portal, and it is posted here when it is done."
	restartedNotice        = "The gateway restarted while *%s* was working. The agent keeps going and its result is in the Dev Portal, but it cannot be posted in this thread. Ask again if you need it here."
	// resumeLostNote replaces the restart notice's promise when the controller
	// no longer has the task the previous process left running.
	resumeLostNote = "The result of the turn that the restart interrupted could not be recovered. Ask again."
	// resumeOnReplyNote replaces the promise when the start-up recovery gave
	// up on the turn (its user signed out, the platform unreachable for as
	// long as the turn may run): the record stays, and a reply delivers it.
	resumeOnReplyNote = "The result of the turn that the restart interrupted could not be posted here automatically. Reply in this thread to get it."
)

func (a *Adapter) restartedNotice(ctx context.Context, agentRef string) string {
	// The note is mrkdwn, and the display name comes from an annotation.
	name := escapeMrkdwn(a.agentNameFor(ctx, agentRef))
	if a.gw.ResumesTurns() {
		return fmt.Sprintf(restartedResumesNotice, name)
	}
	return fmt.Sprintf(restartedNotice, name)
}

// Recovery of a turn: the start-up pass retries a failure that may clear (the
// routing store or the controller not reachable yet, a token mint refused
// while muster rolls alongside the gateway) with a backoff from
// recoverRetryDelay up to recoverRetryMax, for as long as the turn itself may
// still run (recoverWindow, a turn's own maxTurnDuration): a result that is
// still coming is worth waiting for. recoverListTimeout bounds one listing of
// the records.
var (
	recoverRetryDelay = 10 * time.Second
	recoverRetryMax   = 2 * time.Minute
	recoverWindow     = maxTurnDuration
)

const (
	recoverListTimeout = 15 * time.Second
	// recoverLookupTimeout bounds the per-thread record lookup a reply makes
	// on the turn's critical path.
	recoverLookupTimeout = 3 * time.Second
)

// recoverBackoff yields the wait before each further try of a recovery until
// recoverWindow is used up: false once it is, or once ctx ends.
func recoverBackoff() func(ctx context.Context) bool {
	deadline := time.Now().Add(recoverWindow)
	delay := recoverRetryDelay
	return func(ctx context.Context) bool {
		wait := min(delay, time.Until(deadline))
		if wait <= 0 {
			return false
		}
		delay = min(2*delay, recoverRetryMax)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
			return true
		}
	}
}

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
// needs all of them.
func (a *Adapter) RecoverTurns() {
	a.background(a.recoverTurns)
}

func (a *Adapter) recoverTurns(ctx context.Context) {
	var turns []channels.InFlightTurn
	for again := recoverBackoff(); ; {
		lctx, cancel := context.WithTimeout(ctx, recoverListTimeout)
		list, err := a.gw.InFlightTurns(lctx, ChannelName)
		cancel()
		if err == nil {
			turns = list
			break
		}
		a.Logger.Warn("slack: list of turns left running by the previous process unavailable", "error", err)
		if !again(ctx) {
			a.Logger.Warn("slack: turns left running by the previous process not listed; each is delivered on its thread's next reply")
			return
		}
	}
	if len(turns) == 0 {
		return
	}
	a.Logger.Info("slack: recovering turns left running by the previous process", "record", "turns_recover", "count", len(turns))
	for _, turn := range turns {
		a.background(func(ctx context.Context) { a.recoverTurn(ctx, turn) })
	}
}

// recoverTurn delivers one left-running turn, retrying a failure that may
// clear. A turn given up on stays recorded, and the thread is told that its
// next reply brings the result (dispatch's deliverLeftoverTurn): the restart
// notice promised an automatic post this process could not make.
func (a *Adapter) recoverTurn(ctx context.Context, turn channels.InFlightTurn) {
	for again := recoverBackoff(); ; {
		token, outcome := a.recoveryToken(ctx, turn.Msg.Resume[resumeKeyUser], "")
		if outcome == recoverDone {
			if !a.acquireThread(turn.Msg.ThreadID) {
				return // another turn holds the thread; the reply path delivers
			}
			outcome = a.deliverInFlight(ctx, turn, token)
			a.releaseThread(turn.Msg.ThreadID)
		}
		switch {
		case outcome == recoverDone || ctx.Err() != nil:
			return
		case outcome == recoverSkip:
			a.postResumeOnReplyNote(ctx, turn)
			return
		case !again(ctx):
			if ctx.Err() != nil {
				return // shutting down: the next process takes the turn up
			}
			a.Logger.Warn("slack: turn left running by the previous process not recovered; delivered on the thread's next reply",
				"thread", turn.Msg.ThreadID, "task", turn.TaskID)
			a.postResumeOnReplyNote(ctx, turn)
			return
		}
	}
}

// postResumeOnReplyNote tells a thread whose left-running turn this process
// gave up on that a reply brings the result, correcting the restart notice's
// promise of an automatic post.
func (a *Adapter) postResumeOnReplyNote(ctx context.Context, turn channels.InFlightTurn) {
	client := a.agentClient(ctx, turn.Msg.AgentRef)
	a.postTerminalNote(ctx, client, turn.Msg.ChannelID, turn.Msg.ThreadID, resumeOnReplyNote)
}

// deliverLeftoverTurn delivers the result of a turn a previous process left
// running on msg's thread, when the record of one exists, before the reply
// that found it is handled. The caller holds the thread's slot; the delivery
// runs under the recorded user's identity when their token can be minted, and
// under the reply's otherwise. Best effort: a failure leaves the record for a
// later reply.
func (a *Adapter) deliverLeftoverTurn(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser string) {
	lctx, cancel := context.WithTimeout(ctx, recoverLookupTimeout)
	turn, found, err := a.gw.InFlightTurn(lctx, msg)
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
	a.deliverInFlight(ctx, turn, token)
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
// original message, when the record names one), from where the previous
// process left the reply: the answer text it posted is not posted again
// (turn.Delivered). The caller holds the thread's slot. The thread is marked
// active under the recorded user, so plain replies into it are served again
// after the restart.
func (a *Adapter) deliverInFlight(ctx context.Context, turn channels.InFlightTurn, token string) recoverOutcome {
	slackUser := turn.Msg.Resume[resumeKeyUser]
	triggerTS := turn.Msg.Resume[resumeKeyMessage]
	slackChannel := turn.Msg.ChannelID
	threadID := turn.Msg.ThreadID
	msg := turn.Msg
	msg.Subject = slackUser
	msg.BearerToken = token
	msg.MessageID = triggerTS
	var initiator string
	if slackUser != "" {
		initiator = a.accessPolicy().SetInitiator(ctx, slackChannel, threadID, slackUser)
		// A collaborator's turn resubscribes the way it ran: as them, through
		// the thread's share.
		a.applyInstanceOwner(ctx, &msg, threadID, slackUser)
	}

	turnCtx, done := a.registerTurn(ctx, threadID)
	defer done()

	client := a.agentClient(pkga2a.WithForwardedToken(ctx, token), msg.AgentRef)
	deltas, err := a.gw.ResumeTurn(turnCtx, msg, turn.TaskID)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return recoverSkip
		case errors.Is(err, a2apkg.ErrTaskNotFound), pkga2a.IsNotFound(err):
			a.Logger.Warn("slack: turn left running by the previous process is gone at the controller", "thread", threadID, "task", turn.TaskID, "error", err)
			a.postTerminalNote(ctx, client, slackChannel, threadID, resumeLostNote)
			return recoverDone
		default:
			a.Logger.Warn("slack: resubscribe to a turn left running failed", "thread", threadID, "task", turn.TaskID, "error", err)
			return recoverRetry
		}
	}
	a.Logger.Info("slack: delivering a turn left running by the previous process",
		"record", "turn_resume", "agent", msg.AgentRef, "slack_user", slackUser,
		"channel_id", msg.ChannelID, "thread_id", threadID, "task_id", turn.TaskID,
		"delivered_text_len", turn.Delivered.TextLen)
	err = a.streamResponse(turnCtx, client, deltas, msg, slackUser, slackChannel, threadID, triggerTS, initiator, channels.TurnUsage{}, turn.Delivered, nil)
	if err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Warn("slack: delivery of a turn left running failed", "thread", threadID, "task", turn.TaskID, "error", err)
	}
	// streamResponse leaves a corrupt-history failure to the caller's recovery,
	// as runTurn's deferred one does for a turn this process started.
	if isCorruptSessionErr(err) {
		a.takePendingTask(threadID)
		a.recoverCorruptSession(ctx, msg, slackChannel)
	}
	return recoverDone
}
