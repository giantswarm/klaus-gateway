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

// connectorCompletion binds a URL elicitation prompt's Done button to what it
// continues. The link button opens the server's link and continues nothing;
// Done, from the person the prompt is for, or muster's return with the
// elicitation's id, is the accept of the URL elicitation, and the first one
// runs the continuation. Later ones continue nothing.
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
	// elicitationID is the elicitation's id, which muster appends to the
	// return after an approval; empty when it gave none.
	elicitationID string
	// returnKey is the random value the prompt's link carries in the
	// gateway's return (promptLink); "" when the link got no return, and the
	// prompt then continues on Done alone.
	returnKey string
	consented bool
}

// mintConnectorCompletion stores fresh completion state for a Connect prompt
// and returns its opaque ID, which the prompt's link button carries as its
// value.
func (a *Adapter) mintConnectorCompletion(entry connectorCompletion) string {
	stateID := randomToken()
	entry.consented = false
	if entry.elicitationID == "" && entry.signIn != nil {
		entry.elicitationID = entry.signIn.ElicitationID
	}

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

// randomToken is 128 random bits, URL-safe.
func randomToken() string {
	raw := make([]byte, 16)
	// crypto/rand.Read never returns an error (Go 1.24+).
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
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
			a.Logger.Warn("slack: token to decline an elicitation unavailable", "user", entry.slackUser, "error", err)
		} else if err := a.SignIns.Decline(ctx, token, entry.signIn); err != nil {
			a.Logger.Warn("slack: decline elicitation failed", "user", entry.slackUser, "thread", entry.threadTS, "error", err)
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
	a.Logger.Info("slack: elicitation declined", "record", "connector_declined", "user", entry.slackUser, "thread", entry.threadTS, "review", entry.review)
	if responseURL != "" {
		if err := respondURL(ctx, responseURL, entry.threadTS, connectorDeclinedNotice); err != nil {
			a.Logger.Warn("slack: rewrite connect prompt after decline failed", "thread", entry.threadTS, "error", err)
		}
	}
}

// continueAfterConsent runs what a URL elicitation prompt holds once its
// person is done at the link (Done, or muster's return). The prompt is
// replaced at once, since Done says nothing of the outcome: each continuation
// reports it on its own surface, from muster's answer to the retried call. A
// runtime's elicitation resumes its paused task with accept: the runtime
// retries its own call. The gateway's own call is accepted: muster runs it
// when what it asked for is done (a sign-in it waits for, an approval it
// matches) or asks again. Blocking, so it runs on the adapter lifecycle
// context via background.
func (a *Adapter) continueAfterConsent(ctx context.Context, entry connectorCompletion, responseURL string) {
	notice := connectorContinuingNotice
	if entry.signInTask != "" {
		notice = signInContinuingNotice
	}
	if responseURL != "" {
		if err := respondURL(ctx, responseURL, entry.threadTS, notice); err != nil {
			a.Logger.Warn("slack: rewrite connect prompt after consent failed", "thread", entry.threadTS, "error", err)
		}
	}
	switch {
	case entry.signInTask != "":
		a.resumeSignInTask(ctx, entry)
	case entry.conversation != "":
		a.resumeConversationReply(ctx, entry)
	case entry.answer != nil:
		a.resumeDecisionAnswer(ctx, entry)
	case entry.review != "":
		a.resumeTeamReviewDecision(ctx, entry)
	case entry.held != nil:
		a.resumeHeldStart(ctx, entry)
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
		a.Logger.Warn("slack: token to accept an elicitation unavailable", "user", entry.slackUser, "error", err)
		return muster.Result{}, false
	}
	res, err := a.SignIns.Accept(ctx, token, entry.signIn)
	if err != nil {
		a.Logger.Warn("slack: accept elicitation failed", "user", entry.slackUser, "thread", entry.threadTS, "error", err)
		return muster.Result{}, false
	}
	return res, true
}

// resumeHeldStart accepts the sign-in a Session start was asked for, which
// waits for it, then runs the held message as the thread's first turn. The
// replayed start names the same request, so it finds the Session the accepted
// start made. The surfaces settle as delivered only when the accept ran the
// start; otherwise the replayed start settles them or, still asked for the
// sign-in, refreshes them with a new prompt.
func (a *Adapter) resumeHeldStart(ctx context.Context, entry connectorCompletion) {
	if res, ok := a.acceptSignIn(ctx, entry); ok && res.SignIn == nil {
		a.settleSessionConnect(ctx, entry.slackUser, entry.threadTS, sessionConnectDelivered)
	}
	if err := a.replayDispatch(ctx, entry.held.Msg, entry.held.SlackChannel); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Log(ctx, failureLogLevel(err), "slack: held message after Done failed", "user", entry.slackUser, "thread", entry.threadTS, "failure_class", channels.ClassifyFailure(err), "error", err)
		a.postReplayFailureNote(ctx, entry.held.SlackChannel, entry.threadTS, err)
	}
}

// resumeSignInTask resumes a task paused on a runtime's sign-in with accept,
// if it is still paused on it.
func (a *Adapter) resumeSignInTask(ctx context.Context, entry connectorCompletion) {
	if a.pendingSignIn(entry.threadTS, entry.signInTask) == nil {
		a.Logger.Info("slack: Done for a task no longer paused on it", "user", entry.slackUser, "thread", entry.threadTS, "task", entry.signInTask)
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
		a.Logger.Log(ctx, failureLogLevel(err), "slack: resume after Done failed", "user", entry.slackUser, "thread", entry.threadTS, "failure_class", channels.ClassifyFailure(err), "error", err)
		a.postReplayFailureNote(ctx, entry.channel, entry.threadTS, err)
	}
}
