package slack

import (
	"context"
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
	muteEndedNotice          = "Unmuted. I'll reply to messages in this thread again."
	muteAlreadyNotice        = "Already muted."
	muteNoConversationNotice = "There is no conversation with the agent in this thread to mute."
)

// muteThread runs the mute command sent as the message at ts. note posts a
// line in the thread, ephemeral one only the sender sees. A thread with no
// conversation is not given one: no row is written, so the sender does not
// become its initiator.
func (a *Adapter) muteThread(ctx context.Context, ts, slackUser, slackChannel, threadID string, note, ephemeral func(string)) {
	st, err := a.gw.ThreadState(ctx, ChannelName, slackChannel, threadID)
	if err != nil {
		a.Logger.Warn("slack: read thread state for mute failed", "thread", threadID, "error", err)
		ephemeral(storeUnavailableNotice)
		return
	}
	if !st.Found || st.Entry.Initiator == "" {
		ephemeral(muteNoConversationNotice)
		return
	}
	if !a.accessPolicy().Allowed(ctx, slackChannel, threadID, slackUser) {
		note(notPermittedNotice)
		return
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
	case already:
		ephemeral(muteAlreadyNotice)
	case !muted:
		// The conversation ended between the read and the write.
		ephemeral(muteNoConversationNotice)
	default:
		a.Logger.Info("slack: thread muted", "record", "thread_muted", "channel_id", slackChannel, "thread", threadID, "user", slackUser)
		note(muteNotice)
	}
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
