package slack

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// SignInAnswerer answers a muster sign-in as the person: Accept retries the
// call it was asked for with their accept, Decline with their decline.
// *muster.Client satisfies it.
type SignInAnswerer interface {
	Accept(ctx context.Context, bearer string, signIn *muster.SignIn) (muster.Result, error)
	Decline(ctx context.Context, bearer string, signIn *muster.SignIn) error
}

// connectorCompletion binds a Connect prompt's link button to what the
// person's consent continues. The button opens the server's link exactly as
// given; its click, from the person the prompt is for, is their consent to
// open it (the accept of the URL elicitation), and the first one runs the
// continuation. Clicks after it only open the link again.
type connectorCompletion struct {
	slackUser string // raw Slack user ID (U…) the prompt is for
	channel   string // Slack channel the prompt was posted in
	threadTS  string // thread root ("" for a channel-level prompt)
	// signIn is the sign-in muster asked the gateway's own call for: the
	// continuation accepts it, which runs the call again once the person
	// signed in. Nil for a task paused on a runtime's sign-in.
	signIn *muster.SignIn
	// review is the team review the prompt was posted for, when the sign-in
	// was needed to submit a decision: the continuation submits that decision
	// (an approval, or a denial with its reason) as the person.
	review   string
	decision teamReviewDecision
	// answer, set with review naming a decision, is the answer the sign-in
	// was needed for.
	answer *decisionAnswer
	// held, set for the prompt that answered a Session start muster asked a
	// sign-in for, is the message the start was for: the continuation runs it
	// as the thread's first turn.
	held *heldMessage
	// signInTask, set for the prompt of a task paused on a runtime's sign-in,
	// is that task: the continuation resumes it with accept, if it is still
	// paused on it.
	signInTask string
	// conversation, set for the prompt of a conversation reply muster asked
	// a sign-in for, is the conversation's ID and reply the reply: the
	// continuation accepts the sign-in, which delivers it.
	conversation string
	reply        conversationReply
	consented    bool
}

// mintConnectorCompletion stores fresh completion state for a Connect prompt
// and returns its opaque ID, which the prompt's link button carries as its
// value.
func (a *Adapter) mintConnectorCompletion(entry connectorCompletion) string {
	raw := make([]byte, 16)
	// crypto/rand.Read never returns an error (Go 1.24+).
	_, _ = rand.Read(raw)
	stateID := base64.RawURLEncoding.EncodeToString(raw)
	entry.consented = false

	now := time.Now()
	a.connectorCompletionsMu.Lock()
	defer a.connectorCompletionsMu.Unlock()
	if a.connectorCompletions == nil {
		a.connectorCompletions = make(map[string]ttlEntry[connectorCompletion])
	}
	sweepExpired(a.connectorCompletions, now)
	a.connectorCompletions[stateID] = ttlEntry[connectorCompletion]{value: entry, expires: now.Add(connectorCompletionTTL)}
	return stateID
}

// dropConnectorCompletions forgets the completion states named, so the
// buttons that carry them continue nothing.
func (a *Adapter) dropConnectorCompletions(stateIDs []string) {
	a.connectorCompletionsMu.Lock()
	defer a.connectorCompletionsMu.Unlock()
	for _, id := range stateIDs {
		delete(a.connectorCompletions, id)
	}
}

// takeConnectorAnswer records slackUser's answer to stateID's link, their
// consent or their decline. ok is true only for the first click of the person
// the prompt is for, on a state that is still alive.
func (a *Adapter) takeConnectorAnswer(slackUser, stateID string) (connectorCompletion, bool) {
	now := time.Now()
	a.connectorCompletionsMu.Lock()
	defer a.connectorCompletionsMu.Unlock()
	stored, found := a.connectorCompletions[stateID]
	if !found || now.After(stored.expires) || stored.value.slackUser != slackUser || stored.value.consented {
		return connectorCompletion{}, false
	}
	stored.value.consented = true
	a.connectorCompletions[stateID] = stored
	return stored.value, true
}

// handleConnectorConnect is a click on a Connect prompt's link button: Slack
// opens the link, and the click is the person's consent to it. The first click
// of the person the prompt is for runs what the prompt holds; any other is a
// no-op.
func (a *Adapter) handleConnectorConnect(slackUser, stateID, responseURL string) {
	entry, ok := a.takeConnectorAnswer(slackUser, stateID)
	if !ok {
		return
	}
	a.background(func(ctx context.Context) { a.continueAfterConsent(ctx, entry, responseURL) })
}

// handleConnectorDecline is a click on a Connect prompt's Not now: the person
// will not open the link. The first click of the person the prompt is for
// declines the sign-in to muster and drops what the prompt holds; any other
// is a no-op.
func (a *Adapter) handleConnectorDecline(slackUser, stateID, responseURL string) {
	entry, ok := a.takeConnectorAnswer(slackUser, stateID)
	if !ok {
		return
	}
	a.background(func(ctx context.Context) { a.dropAfterDecline(ctx, entry, responseURL) })
}

// dropAfterDecline answers muster's sign-in with decline, then settles what
// the prompt held: a held Session start is not sent, and a decision or a team
// review loses the status line naming the person signing in.
func (a *Adapter) dropAfterDecline(ctx context.Context, entry connectorCompletion, responseURL string) {
	if a.SignIns != nil && a.OBO != nil && entry.signIn != nil {
		if token, err := a.OBO.TokenFor(ctx, entry.slackUser); err != nil {
			a.Logger.Warn("slack: token to decline a sign-in unavailable", "user", entry.slackUser, "error", err)
		} else if err := a.SignIns.Decline(ctx, token, entry.signIn); err != nil {
			a.Logger.Warn("slack: decline sign-in failed", "user", entry.slackUser, "thread", entry.threadTS, "error", err)
		}
	}
	switch {
	case entry.held != nil:
		a.settleSessionConnect(ctx, entry.slackUser, entry.threadTS, sessionConnectDeclined)
	case entry.answer != nil:
		if rv, ok := a.decisionFor(ctx, entry.review, entry.slackUser); ok {
			a.showDecisionStatus(ctx, rv, "")
		}
	case entry.review != "":
		if rv, found, err := a.reviews().GetReview(ctx, entry.review); err == nil && found {
			a.showTeamReviewStatus(ctx, rv, "")
		}
	}
	a.Logger.Info("slack: sign-in declined", "record", "connector_declined", "user", entry.slackUser, "thread", entry.threadTS, "review", entry.review)
	if responseURL != "" {
		if err := respondURL(ctx, responseURL, entry.threadTS, connectorDeclinedNotice); err != nil {
			a.Logger.Warn("slack: rewrite connect prompt after decline failed", "thread", entry.threadTS, "error", err)
		}
	}
}

// continueAfterConsent runs what a Connect prompt holds once its person agreed
// to open the link. A runtime's sign-in resumes its paused task with accept:
// the runtime retries its own call. The gateway's own call is accepted: muster
// waits for the sign-in, then runs the call. Blocking, so it runs on the
// adapter lifecycle context via background.
func (a *Adapter) continueAfterConsent(ctx context.Context, entry connectorCompletion, responseURL string) {
	if entry.signInTask != "" {
		if responseURL != "" {
			if err := respondURL(ctx, responseURL, entry.threadTS, signInContinuingNotice); err != nil {
				a.Logger.Warn("slack: rewrite sign-in prompt after consent failed", "thread", entry.threadTS, "error", err)
			}
		}
		a.resumeSignInTask(ctx, entry)
		return
	}
	switch {
	case entry.conversation != "":
		a.resumeConversationReply(ctx, entry)
	case entry.answer != nil:
		a.resumeDecisionAnswer(ctx, entry)
	case entry.review != "":
		a.resumeTeamReviewDecision(ctx, entry)
	case entry.held != nil:
		a.resumeHeldStart(ctx, entry)
	default:
		return
	}
	if responseURL != "" {
		if err := respondURL(ctx, responseURL, entry.threadTS, connectorDoneNotice); err != nil {
			a.Logger.Warn("slack: rewrite connect prompt after consent failed", "thread", entry.threadTS, "error", err)
		}
	}
}

// acceptSignIn accepts entry's sign-in as its person: muster waits for the
// sign-in, then runs the call it was asked for. ok is false when there was
// nothing to accept or the person's token or the call failed; the failure is
// logged.
func (a *Adapter) acceptSignIn(ctx context.Context, entry connectorCompletion) (muster.Result, bool) {
	if a.SignIns == nil || a.OBO == nil || entry.signIn == nil {
		return muster.Result{}, false
	}
	token, err := a.OBO.TokenFor(ctx, entry.slackUser)
	if err != nil {
		a.Logger.Warn("slack: token to accept a sign-in unavailable", "user", entry.slackUser, "error", err)
		return muster.Result{}, false
	}
	res, err := a.SignIns.Accept(ctx, token, entry.signIn)
	if err != nil {
		a.Logger.Warn("slack: accept sign-in failed", "user", entry.slackUser, "thread", entry.threadTS, "error", err)
		return muster.Result{}, false
	}
	return res, true
}

// resumeHeldStart accepts the sign-in a Session start was asked for, which
// waits for it, then runs the held message as the thread's first turn. The
// replayed start names the same request, so it finds the Session the accepted
// start made; a sign-in that did not complete in time is asked for again by
// that start.
func (a *Adapter) resumeHeldStart(ctx context.Context, entry connectorCompletion) {
	if res, ok := a.acceptSignIn(ctx, entry); !ok || res.SignIn == nil {
		a.settleSessionConnect(ctx, entry.slackUser, entry.threadTS, sessionConnectDelivered)
	}
	if err := a.replayDispatch(ctx, entry.held.Msg, entry.held.SlackChannel); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Log(ctx, failureLogLevel(err), "slack: held message after sign-in failed", "user", entry.slackUser, "thread", entry.threadTS, "failure_class", channels.ClassifyFailure(err), "error", err)
		a.postReplayFailureNote(ctx, entry.held.SlackChannel, entry.threadTS, err)
	}
}

// resumeSignInTask resumes a task paused on a runtime's sign-in with accept,
// if it is still paused on it.
func (a *Adapter) resumeSignInTask(ctx context.Context, entry connectorCompletion) {
	if a.pendingSignIn(entry.threadTS, entry.signInTask) == nil {
		a.Logger.Info("slack: sign-in consent for a task no longer paused on it", "user", entry.slackUser, "thread", entry.threadTS, "task", entry.signInTask)
		return
	}
	msg := channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: entry.channel,
		ThreadID:  entry.threadTS,
		Text:      signInResumeText,
		Subject:   entry.slackUser,
	}
	if err := a.replayDispatch(ctx, msg, entry.channel); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Log(ctx, failureLogLevel(err), "slack: sign-in resume failed", "user", entry.slackUser, "thread", entry.threadTS, "failure_class", channels.ClassifyFailure(err), "error", err)
		a.postReplayFailureNote(ctx, entry.channel, entry.threadTS, err)
	}
}
