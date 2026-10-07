package slack

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// A muted thread is one where the people talk to each other: the agent keeps
// its binding, its session and its context, but answers only a message that
// mentions it. The word mute sets it, from the thread initiator or a
// collaborator; the first message that reaches a turn ends it, and in a muted
// thread only a mention gets there (the inactive-thread gate in handleInbound
// drops the rest). The state is the row's MutedAt, so it survives a restart
// on a persistent store and goes with the row when the conversation ends.

// What a mute and its end say.
const (
	muteNotice               = "Muted. I won't reply here until someone mentions me."
	muteStoppedNotice        = "Stopped and muted. I won't reply here until someone mentions me."
	muteEndedNotice          = "Unmuted. I'll reply to messages in this thread again."
	muteAlreadyNotice        = "Already muted."
	muteNoConversationNotice = "There is no conversation with the agent in this thread to mute."
	muteQuestionOpenNotice   = "The agent asked a question here. Answer it, or mention the agent, before you mute."
	muteParkedDroppedNotice  = "The thread was muted; mention the agent to ask again."
)

// muteThread runs the mute command sent as the message at ts, and reports
// whether it consumed the message. note posts a line in the thread,
// ephemeral one only the sender sees. A thread with no conversation is not
// given one: no row is written, so the sender does not become its initiator.
//
// A busy thread is muted too. A running turn is stopped the way stop stops
// it. An open approval card is rejected the way a typed deny word rejects
// it: the message is left unconsumed, so dispatch takes the paused task and
// reads "mute" as a deny word, the way stop falls through; dispatch does not
// let the word end the mute. An open question refuses
// the mute: a paused task can only be answered, and the word is not that
// answer. A thread already muted is stopped and its card rejected the same
// way; only the note differs.
//
// The messages parked in the thread — for the initiator's consent or for
// their sender's sign-in — are dropped (dropParked), so one written before
// the mute does not run a turn in it later.
func (a *Adapter) muteThread(ctx context.Context, ts, slackUser, slackChannel, threadID string, note, ephemeral func(string)) bool {
	st, err := a.gw.ThreadState(ctx, ChannelName, slackChannel, threadID)
	if err != nil {
		a.Logger.Warn("slack: read thread state for mute failed", "thread", threadID, "error", err)
		ephemeral(storeUnavailableNotice)
		return true
	}
	if !st.Found || st.Entry.Initiator == "" {
		ephemeral(muteNoConversationNotice)
		return true
	}
	if !a.accessPolicy().Allowed(ctx, slackChannel, threadID, slackUser) {
		note(notPermittedNotice)
		return true
	}
	// A thread already muted is answered as such, a question open or not: it
	// can be busy all the same, since the turn that resumes a rejected task
	// runs in it and may ask again.
	if task := a.peekPendingTask(threadID); st.Entry.MutedAt == "" && task != nil && (task.Prompt == nil || task.Prompt.IsAskUser()) {
		ephemeral(muteQuestionOpenNotice)
		return true
	}
	muted, already := false, false
	err = a.gw.UpdateThreadRecord(ctx, ChannelName, slackChannel, threadID, func(e *store.Entry, found bool) bool {
		switch {
		case !found || e.Initiator == "":
			return false
		case e.MutedAt != "":
			already = true
			return false
		}
		e.MutedAt = ts
		muted = true
		return true
	})
	switch {
	case err != nil:
		a.Logger.Warn("slack: write mute failed", "thread", threadID, "error", err)
		ephemeral(storeUnavailableNotice)
		return true
	case !muted && !already:
		// The conversation ended between the read and the write.
		ephemeral(muteNoConversationNotice)
		return true
	}

	// The turn is stopped and the paused task read only after the write, so a
	// turn that ended in between has left its prompt to read here, and an
	// approval it left is rejected all the same. Two windows stay open, both
	// narrow: a turn that ends with a question in between leaves the thread
	// muted beside it (the question still takes its answer, or a mention),
	// and a click that takes the approval before dispatch does leaves "mute"
	// to reach the agent as text, which keeps the mute.
	stopped := a.stopThread(threadID)
	task := a.peekPendingTask(threadID)
	rejects := !stopped && task != nil && task.Prompt != nil && !task.Prompt.IsAskUser()
	senders := a.dropParked(ctx, slackChannel, threadID)
	a.Logger.Info("slack: thread muted", "record", "thread_muted", "channel_id", slackChannel, "thread", threadID,
		"user", slackUser, "already", already, "stopped_turn", stopped, "rejects_approval", rejects,
		"dropped_parked_senders", len(senders))
	for _, sender := range senders {
		if err := a.apiClient().postEphemeralText(ctx, slackChannel, sender, threadID, muteParkedDroppedNotice); err != nil {
			a.Logger.Warn("slack: post parked-dropped notice failed", "thread", threadID, "user", sender, "error", err)
		}
	}
	switch {
	case stopped:
		note(muteStoppedNotice)
	case already:
		ephemeral(muteAlreadyNotice)
	default:
		note(muteNotice)
	}
	return !rejects
}

// dropParked removes the messages parked in threadID, for consent and for
// sign-in, from the process and from the thread's row, and returns their
// senders, each once. It waits for the restore of the held state first: a
// drop before it would leave the row's messages to be read back afterwards
// and replayed. A sign-in that completed in between has nothing to replay,
// on this process or after a restart. One window stays open, narrow: a
// sign-in or an Allow that took its messages just before the drop replays
// them in the muted thread; they are older than the mute, so they run a turn
// but do not end it.
func (a *Adapter) dropParked(ctx context.Context, slackChannel, threadID string) []string {
	if ch := a.heldRestored.Load(); ch != nil {
		select {
		case <-*ch:
		case <-ctx.Done():
			return nil
		}
	}
	senders := map[string]bool{}

	a.pendingLoginMu.Lock()
	for user, threads := range a.pendingLogin {
		if len(threads[threadID]) == 0 {
			continue
		}
		delete(threads, threadID)
		if len(threads) == 0 {
			delete(a.pendingLogin, user)
		}
		senders[user] = true
	}
	a.pendingLoginMu.Unlock()

	a.pendingAccessMu.Lock()
	for user, queue := range a.pendingAccess[threadID] {
		if len(queue) > 0 {
			senders[user] = true
		}
	}
	delete(a.pendingAccess, threadID)
	a.pendingAccessMu.Unlock()

	if len(senders) > 0 {
		a.persistHeld(slackChannel, threadID)
	}
	return slices.Sorted(maps.Keys(senders))
}

// isMuteCommand reports whether msg runs as the mute command. It reads only
// what the process holds, so the gate that asks costs no store call.
func (a *Adapter) isMuteCommand(msg channels.InboundMessage) bool {
	cmd := a.bareCommandFor(msg)
	return cmd != nil && cmd.Name == cmdMute
}

// endMute ends threadID's mute when the message at ts was written after it,
// says so in the thread, and returns the mute's own ts so the turn can catch
// up on what was written since; "" when it ended no mute. A message from
// before the mute — one that passed the gate just before the mute was
// written, or a replay of one parked earlier — leaves it. A store that cannot
// be written leaves the thread muted; the turn still runs. A direct message
// is never muted, so it costs no store call.
func (a *Adapter) endMute(ctx context.Context, slackChannel, threadID, ts string) string {
	if isDMChannelID(slackChannel) {
		return ""
	}
	mutedAt := ""
	err := a.gw.UpdateThreadRecord(ctx, ChannelName, slackChannel, threadID, func(e *store.Entry, found bool) bool {
		if !found || e.MutedAt == "" || !tsAfter(ts, e.MutedAt) {
			return false
		}
		mutedAt, e.MutedAt = e.MutedAt, ""
		return true
	})
	if err != nil {
		a.Logger.Warn("slack: end mute failed; the thread stays muted", "thread", threadID, "error", err)
		return ""
	}
	if mutedAt == "" {
		return ""
	}
	a.Logger.Info("slack: thread unmuted", "record", "thread_unmuted", "channel_id", slackChannel, "thread", threadID)
	if _, err := a.apiClient().postNote(ctx, slackChannel, muteEndedNotice, threadID); err != nil {
		a.Logger.Warn("slack: post unmute note failed", "thread", threadID, "error", err)
	}
	return mutedAt
}

// tsAfter reports whether Slack timestamp ts is later than ref. A timestamp is
// "<seconds>.<micro>", and its parts are compared as numbers: a float would
// round away the microseconds. One that does not parse is not later.
func tsAfter(ts, ref string) bool {
	s1, f1, ok1 := parseTS(ts)
	s2, f2, ok2 := parseTS(ref)
	if !ok1 || !ok2 {
		return false
	}
	return s1 > s2 || (s1 == s2 && f1 > f2)
}

// parseTS splits a Slack timestamp into its seconds and its microseconds.
func parseTS(ts string) (sec, micro int64, ok bool) {
	whole, frac, _ := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || len(frac) > 6 {
		return 0, 0, false
	}
	if frac == "" {
		return sec, 0, true
	}
	micro, err = strconv.ParseInt(frac+strings.Repeat("0", 6-len(frac)), 10, 64)
	return sec, micro, err == nil
}
