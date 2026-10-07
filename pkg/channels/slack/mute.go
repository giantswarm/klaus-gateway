package slack

import (
	"context"

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

// endMute ends threadID's mute, if it has one, and says so in the thread. A
// store that cannot be written leaves the thread muted; the turn still runs.
// A direct message is never muted, so it costs no store call.
func (a *Adapter) endMute(ctx context.Context, slackChannel, threadID string) {
	if isDMChannelID(slackChannel) {
		return
	}
	ended := false
	err := a.gw.UpdateThreadRecord(ctx, ChannelName, slackChannel, threadID, func(e *store.Entry, found bool) bool {
		if !found || e.MutedAt == "" {
			return false
		}
		e.MutedAt = ""
		ended = true
		return true
	})
	if err != nil {
		a.Logger.Warn("slack: end mute failed; the thread stays muted", "thread", threadID, "error", err)
		return
	}
	if !ended {
		return
	}
	a.Logger.Info("slack: thread unmuted", "record", "thread_unmuted", "channel_id", slackChannel, "thread", threadID)
	if _, err := a.apiClient().postNote(ctx, slackChannel, muteEndedNotice, threadID); err != nil {
		a.Logger.Warn("slack: post unmute note failed", "thread", threadID, "error", err)
	}
}
