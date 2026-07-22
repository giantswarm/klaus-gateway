package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// pendingTask records the A2A task paused at input-required for a thread.
type pendingTask struct {
	TaskID    string
	AgentRef  string
	Channel   string // Slack channel ID for posting the resumed response
	ChannelID string // logical channel ID used in the routing key
	// SlackUser is the user whose turn paused on this task. Only they may
	// resolve it: the resume runs under the answering user's token, and the
	// paused task lives in its owner's agent session, so an answer from anyone
	// else would land in the wrong session. Empty skips the check.
	SlackUser string
	// Prompt is the structured approval request the task is paused on, used to
	// map a free-text reply or choice click back to a HITL decision.
	Prompt *channels.HitlPrompt
	// Usage carries the paused turn's token counts so the resuming turn reports
	// the whole turn, not just its tail.
	Usage channels.TurnUsage

	storedAt time.Time // set by storePendingTask; drives the TTL sweep
}

// pendingAccessReq is a newcomer's message parked while the thread initiator is
// asked to approve them. Parked per (thread, user) as an ordered queue and
// replayed in order through dispatch on approval.
type pendingAccessReq struct {
	msg          channels.InboundMessage
	slackChannel string

	storedAt time.Time // set by storePendingAccess; drives the TTL sweep
}

// pendingLoginReq is a message parked while its unlinked sender completes the
// sign-in flow. It is replayed through dispatch when the user links, so the
// question they typed is answered without them having to send it again. Parked
// per (user, thread) as an ordered queue: several messages in one thread are
// kept and replayed in order, and messages in other threads are replayed too.
type pendingLoginReq struct {
	msg          channels.InboundMessage
	slackChannel string

	storedAt time.Time // set by parkPendingLogin; drives the TTL sweep
}

// maxParkedPerThread bounds each parked queue (login and access) per (user,
// thread) so a chatty burst does not replay an unbounded string of turns. When
// exceeded the oldest message is dropped, keeping the most recent ones in order.
const maxParkedPerThread = 5

// pendingTTL bounds how long a parked task or access request is kept. Both
// maps hold user content and grow per thread; an entry this old is abandoned
// (the paused A2A task has long been resumable by nobody) and is swept on the
// next store.
const pendingTTL = 24 * time.Hour

// storePendingAccess appends a newcomer's message to their parked queue for the
// thread while the initiator is asked to approve them. Bounded per (thread,
// user) by maxParkedPerThread (oldest dropped past the cap). first is true when
// this is the first parked request for the (thread, user), so the caller posts
// the consent prompt once rather than on every parked message (e.g. a burst
// replayed after sign-in); dropped is true when the cap evicted a message, so
// the caller can surface the loss.
func (a *Adapter) storePendingAccess(threadID, userID string, req *pendingAccessReq) (first, dropped bool) {
	a.pendingAccessMu.Lock()
	defer a.pendingAccessMu.Unlock()
	if a.pendingAccess == nil {
		a.pendingAccess = make(map[string]map[string][]*pendingAccessReq)
	}
	byUser := a.pendingAccess[threadID]
	if byUser == nil {
		byUser = make(map[string][]*pendingAccessReq)
		a.pendingAccess[threadID] = byUser
	}
	existed := len(byUser[userID]) > 0
	req.storedAt = time.Now()
	queue := append(byUser[userID], req)
	if len(queue) > maxParkedPerThread {
		queue = queue[len(queue)-maxParkedPerThread:]
		dropped = true
	}
	byUser[userID] = queue
	for thread, users := range a.pendingAccess {
		for user, q := range users {
			kept := q[:0]
			for _, r := range q {
				if time.Since(r.storedAt) <= pendingTTL {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				delete(users, user)
			} else {
				users[user] = kept
			}
		}
		if len(users) == 0 {
			delete(a.pendingAccess, thread)
		}
	}
	return !existed, dropped
}

// takePendingAccess atomically retrieves and removes a user's parked messages
// for the thread, in arrival order. Entries past pendingTTL are dropped.
func (a *Adapter) takePendingAccess(threadID, userID string) []*pendingAccessReq {
	a.pendingAccessMu.Lock()
	defer a.pendingAccessMu.Unlock()
	byUser := a.pendingAccess[threadID]
	if byUser == nil {
		return nil
	}
	queue := byUser[userID]
	delete(byUser, userID)
	if len(byUser) == 0 {
		delete(a.pendingAccess, threadID)
	}
	fresh := queue[:0]
	for _, r := range queue {
		if time.Since(r.storedAt) <= pendingTTL {
			fresh = append(fresh, r)
		}
	}
	return fresh
}

// parkPendingLogin appends a message to the user's parked queue for its thread
// so it can be replayed once the link completes. Bounded per thread by
// maxParkedPerThread (oldest dropped past the cap); dropped reports an
// eviction so the caller can surface the loss. Abandoned entries are swept
// opportunistically.
func (a *Adapter) parkPendingLogin(slackUser string, req *pendingLoginReq) (dropped bool) {
	a.pendingLoginMu.Lock()
	defer a.pendingLoginMu.Unlock()
	if a.pendingLogin == nil {
		a.pendingLogin = make(map[string]map[string][]*pendingLoginReq)
	}
	byThread := a.pendingLogin[slackUser]
	if byThread == nil {
		byThread = make(map[string][]*pendingLoginReq)
		a.pendingLogin[slackUser] = byThread
	}
	req.storedAt = time.Now()
	queue := append(byThread[req.msg.ThreadID], req)
	if len(queue) > maxParkedPerThread {
		queue = queue[len(queue)-maxParkedPerThread:]
		dropped = true
	}
	byThread[req.msg.ThreadID] = queue
	for user, threads := range a.pendingLogin {
		for thread, q := range threads {
			kept := q[:0]
			for _, r := range q {
				if time.Since(r.storedAt) <= pendingTTL {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				delete(threads, thread)
			} else {
				threads[thread] = kept
			}
		}
		if len(threads) == 0 {
			delete(a.pendingLogin, user)
		}
	}
	return dropped
}

// notifyParkedDrop tells a user (ephemerally, once per (user, thread) per
// window) that parked messages past the cap were dropped, so they know to
// resend rather than assuming everything was held. Best-effort.
func (a *Adapter) notifyParkedDrop(ctx context.Context, slackChannel, threadID, slackUser string) {
	key := slackUser + "\x00" + threadID
	if !markOnce(&a.parkedDropNoticedMu, &a.parkedDropNoticed, key, parkedDropNoticeTTL) {
		return
	}
	text := fmt.Sprintf(parkedDropNotice, maxParkedPerThread)
	if err := a.apiClient().postEphemeralText(ctx, slackChannel, slackUser, threadID, text); err != nil {
		a.Logger.Warn("slack: post parked-drop notice failed", "user", slackUser, "thread", threadID, "error", err)
	}
}

// parkForLogin parks msg for replay after sign-in and posts the (throttled)
// sign-in prompt. The TokenFor miss that brought the caller here and the park
// are not atomic: a link callback firing in between would find nothing to
// drain and leave the parked message stranded until the TTL sweep. Re-checking
// after the park closes that window; when the user turns out to be linked the
// queue is drained immediately through the normal replay path instead of
// prompting.
func (a *Adapter) parkForLogin(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser string) {
	if a.parkPendingLogin(slackUser, &pendingLoginReq{msg: msg, slackChannel: slackChannel}) {
		a.notifyParkedDrop(ctx, slackChannel, msg.ThreadID, slackUser)
	}
	if _, err := a.OBO.TokenFor(ctx, slackUser); err == nil {
		a.OnUserLinked(ctx, slackUser, "")
		return
	}
	a.maybePostSignIn(ctx, slackChannel, msg.ThreadID, slackUser)
}

// takePendingLogin atomically retrieves and removes a user's parked messages,
// grouped by thread and kept in arrival order within each thread. Entries past
// pendingTTL are dropped, so an abandoned sign-in completed a day later does
// not replay stale questions.
func (a *Adapter) takePendingLogin(slackUser string) map[string][]*pendingLoginReq {
	a.pendingLoginMu.Lock()
	defer a.pendingLoginMu.Unlock()
	byThread := a.pendingLogin[slackUser]
	delete(a.pendingLogin, slackUser)
	for thread, queue := range byThread {
		kept := queue[:0]
		for _, r := range queue {
			if time.Since(r.storedAt) <= pendingTTL {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			delete(byThread, thread)
		} else {
			byThread[thread] = kept
		}
	}
	if len(byThread) == 0 {
		return nil
	}
	return byThread
}

// OnUserLinked rewrites the sign-in prompt message(s) into a confirmation and
// replays the messages the user sent before signing in, once their muster
// identity is linked. Registered as the musterlink OnLinked hook, whose contract
// is that it must not block: HandleCallback renders the "signed in, close this
// tab" page only after it returns. So both the prompt rewrite and the replay
// are dispatched on their own goroutines and this returns promptly. Each thread's
// queue is replayed in order on its own goroutine (dispatch serializes per
// thread, so ordering only holds within a sequential drain); threads replay
// concurrently.
//
// The background work runs on the adapter lifecycle context, not the OAuth
// callback context: the callback context is request-scoped, so a shutdown would
// not cancel work dispatched from it, whereas normal dispatch (on the lifecycle
// context) is cancelled. Falls back to the passed context when the adapter was
// constructed without Start (direct-construction tests).
func (a *Adapter) OnUserLinked(ctx context.Context, slackUser, email string) {
	bgCtx := a.baseCtx
	if bgCtx == nil {
		bgCtx = ctx
	}
	queues := a.takePendingLogin(slackUser)
	// The prompt rewrite announces the agent handoff only when a replay will
	// actually reach the agent: bare auth utterances are satisfied by the link
	// itself, and a newcomer's replay lands at the initiator's consent prompt
	// instead, so both keep the plain confirmation.
	replayingThreads := make(map[string]bool)
	for threadID, queue := range queues {
		for _, req := range queue {
			if !isBareAuthUtterance(req.msg.Text) && a.accessPolicy().Allowed(threadID, slackUser) {
				replayingThreads[threadID] = true
			}
		}
	}
	// updateSignInAnchors does blocking chat.update round-trips; running them
	// inline would delay the sign-in success page. Ordering against replay is
	// immaterial: the prompt and the agent's thread reply are independent
	// messages.
	go a.updateSignInAnchors(bgCtx, slackUser, replayingThreads)
	for _, queue := range queues {
		go func() {
			for _, req := range queue {
				// A bare "login"-style message asked for the sign-in that just
				// completed; replaying it would send a stale request to the agent.
				if isBareAuthUtterance(req.msg.Text) {
					a.Logger.Debug("slack: dropping parked sign-in request, link satisfied it",
						"user", slackUser, "thread", req.msg.ThreadID)
					continue
				}
				if err := a.replayDispatch(bgCtx, req.msg, req.slackChannel); err != nil && !errors.Is(err, context.Canceled) {
					a.Logger.Error("slack: replay after sign-in failed", "user", slackUser, "thread", req.msg.ThreadID, "error", err)
					a.postReplayFailureNote(bgCtx, req.slackChannel, req.msg.ThreadID)
				}
			}
		}()
	}
}

// authUtterances are messages that are nothing but a request to sign in. A
// parked one is satisfied by the link completing, so it is dropped at replay
// instead of confusing the agent with a stale "login".
var authUtterances = map[string]struct{}{
	cmdLogin:       {},
	"log in":       {},
	"sign in":      {},
	"signin":       {},
	"connect":      {},
	"/" + cmdLogin: {},
}

// isBareAuthUtterance reports whether text, mention-stripped and normalized,
// exactly matches an auth utterance (optionally with trailing punctuation).
// Never a substring match: "login to grafana is broken" must replay.
func isBareAuthUtterance(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(StripMention(text)))
	normalized = strings.TrimSpace(strings.TrimRight(normalized, ".!?"))
	_, ok := authUtterances[normalized]
	return ok
}

// storePendingTask records a paused input-required task for a thread.
// Any existing pending task for that thread is replaced. Abandoned entries are
// swept opportunistically so the map does not grow for the process lifetime.
func (a *Adapter) storePendingTask(threadID string, task *pendingTask) {
	a.pendingMu.Lock()
	if a.pendingTasks == nil {
		a.pendingTasks = make(map[string]*pendingTask)
	}
	task.storedAt = time.Now()
	a.pendingTasks[threadID] = task
	for thread, t := range a.pendingTasks {
		if time.Since(t.storedAt) > pendingTTL {
			delete(a.pendingTasks, thread)
		}
	}
	a.pendingMu.Unlock()
}

// takePendingTask atomically retrieves and removes a pending task for a thread.
// Returns nil when no task is pending.
func (a *Adapter) takePendingTask(threadID string) *pendingTask {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	task := a.pendingTasks[threadID]
	delete(a.pendingTasks, threadID)
	return task
}

// hasPendingTask reports whether a thread has a pending input-required task.
func (a *Adapter) hasPendingTask(threadID string) bool {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	_, ok := a.pendingTasks[threadID]
	return ok
}

// peekPendingTask returns a thread's pending task without removing it, or nil
// when none is pending. A caller that relies on the task still being pending on
// a later takePendingTask must hold the thread lock across both.
func (a *Adapter) peekPendingTask(threadID string) *pendingTask {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	return a.pendingTasks[threadID]
}
