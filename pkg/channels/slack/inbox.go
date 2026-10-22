package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// A message for a thread's conversation waits in the thread's inbox
// (heldState.Inbox) until it runs. The row is written before Slack is
// answered, so a gateway that dies between the answer and the turn loses
// nothing, and a message that arrives while a turn runs runs after it instead
// of being turned away. The inbox is in the row only: the process tracks the
// drainer that runs it (inboxState). One drainer per thread runs the queue in
// order through dispatch, which decides the rest when the message runs:
// access, sign-in, a workspace prompt.

// maxQueued bounds the messages that wait in one thread's inbox.
const maxQueued = 5

// inboxWriteTimeout bounds one write that takes a handled message out of the
// inbox; it runs after the turn, also through the gateway's shutdown.
const inboxWriteTimeout = 5 * time.Second

// What the inbox tells the sender of a message, privately.
const (
	queuedNotice    = "Queued. It runs after the reply in progress."
	queueFullNotice = "%d messages already wait for the agent in this thread, so this one was not queued. Send it again once they have run."
)

// queuedMessage is a message in a thread's inbox, kept as a parked message is
// kept, with the Slack event it arrived with.
type queuedMessage struct {
	heldMessage
	EventID string `json:"event_id,omitempty"`
}

// inboxState is what the process tracks of a thread's inbox.
type inboxState struct {
	slackChannel string
	// draining is set while a drainer runs the inbox. again makes it read the
	// inbox once more before it stops: a message was queued, or a turn ended,
	// meanwhile.
	draining, again bool
	// running is the message the drainer has handed to dispatch.
	running string
	// paused is set when the drainer stopped with messages left behind a
	// prompt the thread waits on: the end of a turn starts it again
	// (wakeInbox), since that turn may have answered the prompt.
	paused bool
}

// queueOutcome is what queueing one message came to.
type queueOutcome int

const (
	queueFailed         queueOutcome = iota // the row could not be written
	queueNoConversation                     // the thread has no conversation: nothing was written
	queueRunsNext                           // queued with nothing ahead of it
	queueWaits                              // queued behind a running turn or earlier messages
	queueDuplicate                          // queued already: a redelivery
	queueFull                               // not queued: maxQueued messages wait already
)

// queueMessage writes msg into its thread's inbox: the store write a message
// for a conversation costs before Slack is answered. It returns what follows
// the answer: the sender is told when the message waits or was not queued, and
// the inbox runs. A thread without a conversation gets nothing written; its
// message goes to dispatch, which answers it with the help.
func (a *Adapter) queueMessage(ctx context.Context, msg channels.InboundMessage, slackChannel, eventID string) func(context.Context) {
	outcome := a.appendQueued(ctx, msg, slackChannel, eventID)
	return func(ctx context.Context) { a.afterQueued(ctx, msg, slackChannel, eventID, outcome) }
}

// appendQueued appends msg to its thread's inbox in the row. A thread waiting
// on a prompt with no turn running takes the message as the prompt's answer
// (nextQueued), so the bound does not refuse it.
func (a *Adapter) appendQueued(ctx context.Context, msg channels.InboundMessage, slackChannel, eventID string) queueOutcome {
	threadID := msg.ThreadID
	busy, running := a.turnRunning(threadID), a.queuedRunning(threadID)
	pending := a.hasPendingTask(threadID)
	answers := pending && !busy
	now := time.Now()
	outcome := queueFailed
	err := a.gw.UpdateThreadRecord(ctx, ChannelName, slackChannel, threadID, func(e *store.Entry, found bool) bool {
		if (!found || e.AgentRef == "") && !pending {
			outcome = queueNoConversation
			return false
		}
		h := a.heldOf(threadID, e.Held)
		waiting := 0
		for _, m := range h.Inbox {
			if m.Msg.MessageID == msg.MessageID {
				outcome = queueDuplicate
				return false
			}
			if m.Msg.MessageID != running && now.Sub(m.StoredAt) <= pendingTTL {
				waiting++
			}
		}
		if waiting >= maxQueued && !answers {
			outcome = queueFull
			return false
		}
		h.Inbox = append(h.Inbox, queuedMessage{heldMessage: heldMessageOf(msg, slackChannel, now), EventID: eventID})
		held, err := h.marshal()
		if err != nil {
			a.Logger.Warn("slack: encode held thread state failed", "thread", threadID, "error", err)
			outcome = queueFailed
			return false
		}
		e.Held = held
		outcome = queueRunsNext
		if busy || (waiting > 0 && !answers) {
			outcome = queueWaits
		}
		return true
	})
	if err != nil {
		a.Logger.Warn("slack: message not queued, the thread state could not be written", "thread", threadID, "user", msg.Subject, "error", err)
		return queueFailed
	}
	return outcome
}

// afterQueued is what follows a message's queueing once Slack has its answer.
func (a *Adapter) afterQueued(ctx context.Context, msg channels.InboundMessage, slackChannel, eventID string, outcome queueOutcome) {
	if outcome == queueNoConversation {
		err := a.dispatch(ctx, msg, slackChannel)
		if !errors.Is(err, errThreadBusy) {
			a.logDispatchError(ctx, slackChannel, msg.ThreadID, err)
			return
		}
		// A conversation opened between the two reads of the row, and its
		// first turn holds the thread: the message waits in it.
		if outcome = a.appendQueued(ctx, msg, slackChannel, eventID); outcome == queueNoConversation {
			outcome = queueFailed
		}
	}
	switch outcome {
	case queueFailed:
		a.tellSender(ctx, msg, slackChannel, storeUnavailableNotice)
		return
	case queueFull:
		a.Logger.Info("slack: message not queued, the thread's inbox is full", "record", "message_queue_full",
			"channel_id", slackChannel, "thread_id", msg.ThreadID, "slack_user", msg.Subject, "message_id", msg.MessageID)
		a.tellSender(ctx, msg, slackChannel, fmt.Sprintf(queueFullNotice, maxQueued))
		return
	case queueWaits:
		a.Logger.Info("slack: message queued behind the thread's turn", "record", "message_queued",
			"channel_id", slackChannel, "thread_id", msg.ThreadID, "slack_user", msg.Subject, "message_id", msg.MessageID, "event_id", eventID)
		a.tellSender(ctx, msg, slackChannel, queuedNotice)
	}
	a.drainInbox(ctx, slackChannel, msg.ThreadID)
}

// tellSender tells msg's sender text, privately, in msg's thread.
func (a *Adapter) tellSender(ctx context.Context, msg channels.InboundMessage, slackChannel, text string) {
	if err := a.apiClient().postEphemeralText(ctx, slackChannel, msg.Subject, msg.ThreadID, text); err != nil {
		a.Logger.Warn("slack: post inbox notice failed", "thread", msg.ThreadID, "user", msg.Subject, "error", err)
	}
}

// logDispatchError logs what dispatch refused or failed on.
func (a *Adapter) logDispatchError(ctx context.Context, slackChannel, threadID string, err error) {
	switch {
	case err == nil:
	case isShareRefusal(err):
		a.Logger.Info("slack: collaborator turn refused", "channel", slackChannel, "thread", threadID, "error", err)
	case !errors.Is(err, context.Canceled):
		// An agent the sender may not use is a refusal, not a failure.
		a.Logger.Log(ctx, failureLogLevel(err), "slack: dispatch error", "channel", slackChannel, "failure_class", channels.ClassifyFailure(err), "error", err)
	}
}

// kickInbox runs threadID's inbox on a goroutine of its own.
func (a *Adapter) kickInbox(slackChannel, threadID string) {
	a.background(func(ctx context.Context) { a.drainInbox(ctx, slackChannel, threadID) })
}

// wakeInbox starts threadID's drainer again when it stopped with messages
// behind a prompt. releaseThread calls it at the end of every turn: the turn
// may have answered the prompt. A drainer still running reads the inbox once
// more before it stops, since it may have read the prompt before the turn.
func (a *Adapter) wakeInbox(threadID string) {
	a.inboxMu.Lock()
	st := a.inboxes[threadID]
	wake, slackChannel := false, ""
	switch {
	case st == nil:
	case st.draining:
		st.again = true
	case st.paused:
		st.paused, wake, slackChannel = false, true, st.slackChannel
	}
	a.inboxMu.Unlock()
	if wake {
		a.kickInbox(slackChannel, threadID)
	}
}

// drainInbox runs threadID's queued messages in order, one at a time, until
// none can run: the inbox is empty, or what is left waits behind a prompt. One
// drainer runs per thread; a call while one runs makes that one read the inbox
// again. It blocks for as long as the turns it runs. A message leaves the row
// once dispatch is done with it. One that the gateway's shutdown cut short
// stays: the next process runs it again, or skips it when the row records its
// turn as left running, which the restart's recovery delivers.
func (a *Adapter) drainInbox(ctx context.Context, slackChannel, threadID string) {
	if !a.claimInbox(slackChannel, threadID) {
		return
	}
	settled := false
	defer func() {
		if !settled {
			a.abandonInbox(threadID)
		}
	}()
	// A queued message runs beside what the previous process held for the
	// thread, a paused prompt above all: it waits until that is read back.
	if ch := a.heldRestored.Load(); ch != nil {
		select {
		case <-*ch:
		case <-ctx.Done():
			return
		}
	}
	var retry func(context.Context) bool
	for ctx.Err() == nil {
		next, ok, behind, err := a.nextQueued(ctx, slackChannel, threadID)
		if err != nil {
			a.Logger.Warn("slack: thread inbox not read", "thread", threadID, "error", err)
			if retry == nil {
				retry = recoverBackoff()
			}
			if !retry(ctx) {
				return
			}
			continue
		}
		if !ok {
			if settled = a.settleInbox(threadID, behind); settled {
				return
			}
			continue
		}
		a.setInboxRunning(threadID, next.Msg.MessageID)
		err = a.dispatch(ctx, next.Msg, next.SlackChannel)
		a.setInboxRunning(threadID, "")
		if errors.Is(err, errThreadBusy) {
			// Another turn holds the thread: a click's resume, a replay, a
			// turn the restart recovers. The inbox waits for it.
			idle := make(chan struct{})
			a.whenThreadIdle(threadID, func() { close(idle) })
			select {
			case <-idle:
			case <-ctx.Done():
				return
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		a.logDispatchError(ctx, next.SlackChannel, threadID, err)
		if !a.unqueueHandled(ctx, slackChannel, threadID, next.Msg.MessageID) {
			return
		}
	}
}

// nextQueued reads threadID's inbox and returns the message that runs next:
// the oldest, or, while the thread waits on a prompt, the oldest written after
// the prompt, which answers it; the ones before it wait for the turn the
// answer runs, and behind reports them. On the way it takes out of the row the
// messages that expired, the ones written before the thread's mute, whose
// senders are told as the mute tells them (a mute stops the running turn
// before it drops the queue), and the one a previous process turned into the
// turn the row records in flight: the restart's recovery delivers that one.
func (a *Adapter) nextQueued(ctx context.Context, slackChannel, threadID string) (next queuedMessage, ok, behind bool, err error) {
	prompt := a.peekPendingTask(threadID)
	now := time.Now()
	var muted []string
	mutedAt := ""
	err = a.gw.UpdateThreadRecord(ctx, ChannelName, slackChannel, threadID, func(e *store.Entry, found bool) bool {
		next, ok, behind, muted, mutedAt = queuedMessage{}, false, false, nil, e.MutedAt
		if !found {
			return false
		}
		h := a.heldOf(threadID, e.Held)
		kept := make([]queuedMessage, 0, len(h.Inbox))
		for _, m := range h.Inbox {
			switch {
			case now.Sub(m.StoredAt) > pendingTTL:
				a.Logger.Info("slack: queued message expired", "record", "message_queue_expired",
					"thread_id", threadID, "slack_user", m.Msg.Subject, "message_id", m.Msg.MessageID)
			case e.MutedAt != "" && tsAfter(e.MutedAt, m.Msg.MessageID):
				muted = append(muted, m.Msg.Subject)
			case e.TaskID != "" && e.Resume[resumeKeyMessage] == m.Msg.MessageID:
				a.Logger.Info("slack: queued message runs as the turn left running; its delivery takes it",
					"thread_id", threadID, "task_id", e.TaskID, "message_id", m.Msg.MessageID)
			default:
				kept = append(kept, m)
			}
		}
		for _, m := range kept {
			if prompt == nil || m.StoredAt.After(prompt.storedAt) {
				next, ok = m, true
				break
			}
		}
		behind = !ok && len(kept) > 0
		if len(kept) == len(h.Inbox) {
			return false
		}
		h.Inbox = kept
		held, err := h.marshal()
		if err != nil {
			a.Logger.Warn("slack: encode held thread state failed", "thread", threadID, "error", err)
			return false
		}
		e.Held = held
		return true
	})
	if err == nil {
		for _, sender := range muted {
			a.tellParkedDropped(ctx, slackChannel, threadID, mutedAt, sender)
		}
	}
	return next, ok, behind, err
}

// unqueueHandled takes a handled message out of the inbox, retrying a store
// that fails: a message left there runs again. False when it gave up.
func (a *Adapter) unqueueHandled(ctx context.Context, slackChannel, threadID, ts string) bool {
	var retry func(context.Context) bool
	for {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inboxWriteTimeout)
		err := a.unqueue(wctx, slackChannel, threadID, ts)
		cancel()
		if err == nil {
			return true
		}
		a.Logger.Warn("slack: a handled message stays in the thread inbox, it runs again unless it is taken out", "thread", threadID, "message_id", ts, "error", err)
		if retry == nil {
			retry = recoverBackoff()
		}
		if !retry(ctx) {
			return false
		}
	}
}

// unqueue takes the message at ts out of threadID's inbox in the row, and
// leaves the rest of the row's held state as it is.
func (a *Adapter) unqueue(ctx context.Context, slackChannel, threadID, ts string) error {
	return a.gw.UpdateThreadRecord(ctx, ChannelName, slackChannel, threadID, func(e *store.Entry, found bool) bool {
		if !found {
			return false
		}
		h := a.heldOf(threadID, e.Held)
		n := len(h.Inbox)
		h.Inbox = slices.DeleteFunc(h.Inbox, func(m queuedMessage) bool { return m.Msg.MessageID == ts })
		if len(h.Inbox) == n {
			return false
		}
		held, err := h.marshal()
		if err != nil {
			a.Logger.Warn("slack: encode held thread state failed", "thread", threadID, "error", err)
			return false
		}
		e.Held = held
		return true
	})
}

// unqueueDelivered takes the message a left-running turn was started by out
// of its thread's inbox before the turn is delivered, so the inbox does not
// run it again once the delivery clears the row's record of the turn.
func (a *Adapter) unqueueDelivered(ctx context.Context, slackChannel, threadID, ts string) {
	if ts == "" {
		return
	}
	if err := a.unqueue(ctx, slackChannel, threadID, ts); err != nil {
		a.Logger.Warn("slack: the message of a turn left running stays in the thread inbox", "thread", threadID, "message_id", ts, "error", err)
	}
}

// claimInbox makes the caller threadID's drainer, unless one runs: that one
// reads the inbox again before it stops, and the caller returns.
func (a *Adapter) claimInbox(slackChannel, threadID string) bool {
	a.inboxMu.Lock()
	defer a.inboxMu.Unlock()
	st := a.inboxes[threadID]
	if st == nil {
		st = &inboxState{}
		if a.inboxes == nil {
			a.inboxes = make(map[string]*inboxState)
		}
		a.inboxes[threadID] = st
	}
	st.slackChannel = slackChannel
	if st.draining {
		st.again = true
		return false
	}
	st.draining, st.again, st.paused = true, false, false
	return true
}

// settleInbox ends the drainer's run unless the inbox may have changed since
// it read it (again), and reports whether it did. behind keeps the thread's
// state for wakeInbox: messages wait behind a prompt.
func (a *Adapter) settleInbox(threadID string, behind bool) bool {
	a.inboxMu.Lock()
	defer a.inboxMu.Unlock()
	st := a.inboxes[threadID]
	if st.again {
		st.again = false
		return false
	}
	st.draining = false
	if behind {
		st.paused = true
	} else {
		delete(a.inboxes, threadID)
	}
	return true
}

// abandonInbox ends a drainer's run that stopped short (the shutdown, a store
// that stayed down): the next message, or the next process, runs the inbox.
func (a *Adapter) abandonInbox(threadID string) {
	a.inboxMu.Lock()
	defer a.inboxMu.Unlock()
	delete(a.inboxes, threadID)
}

// setInboxRunning records the message the drainer has handed to dispatch, ""
// once dispatch returned.
func (a *Adapter) setInboxRunning(threadID, ts string) {
	a.inboxMu.Lock()
	defer a.inboxMu.Unlock()
	if st := a.inboxes[threadID]; st != nil {
		st.running = ts
	}
}

// turnRunning reports whether a turn holds threadID's slot.
func (a *Adapter) turnRunning(threadID string) bool {
	busy := false
	a.withThread(threadID, func(st *threadState) { busy = st.slot != nil })
	return busy
}

// queuedRunning is the queued message threadID's drainer has handed to
// dispatch, "" for none.
func (a *Adapter) queuedRunning(threadID string) string {
	a.inboxMu.Lock()
	defer a.inboxMu.Unlock()
	if st := a.inboxes[threadID]; st != nil {
		return st.running
	}
	return ""
}
