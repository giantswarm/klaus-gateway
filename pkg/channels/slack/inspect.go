package slack

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// maxToolLogEntries bounds the retained tool-call log per thread: the last N
// rendered entries are kept, older ones are dropped (and counted, so the
// inspection reply can say so). Together with the threadStateTTL sweep this
// hard-bounds the log's memory on a long-lived pod. The log is in-memory only
// and lost on restart by design.
const maxToolLogEntries = 100

// toolLogEntry is one retained tool call or result rendering, in the
// detailsFull entry format (already escaped for an mrkdwn context block).
type toolLogEntry struct {
	turn int    // 1-based turn ordinal within this thread's log
	md   string // rendered entry: "🔧 `name`" + args span, or "↳ `name` result" + preview
}

// threadToolLog is one thread's retained tool activity. turns counts turns
// recorded since the log entry was created; dropped counts entries evicted by
// the per-thread cap, so the inspection reply is honest about what it shows.
type threadToolLog struct {
	turns   int
	dropped int
	entries []toolLogEntry
}

// beginToolLogTurn opens a new turn in threadID's tool log and returns its
// ordinal. Called once per turn, on the first tool call the stream records.
func (a *Adapter) beginToolLogTurn(threadID string) int {
	a.toolLogMu.Lock()
	defer a.toolLogMu.Unlock()
	log := a.touchToolLogLocked(threadID)
	log.turns++
	return log.turns
}

// appendToolLog retains one rendered tool entry for threadID under the given
// turn ordinal, evicting the oldest entries past the per-thread cap.
func (a *Adapter) appendToolLog(threadID string, turn int, md string) {
	a.toolLogMu.Lock()
	defer a.toolLogMu.Unlock()
	log := a.touchToolLogLocked(threadID)
	log.entries = append(log.entries, toolLogEntry{turn: turn, md: md})
	if over := len(log.entries) - maxToolLogEntries; over > 0 {
		log.dropped += over
		log.entries = append(log.entries[:0], log.entries[over:]...)
	}
}

// touchToolLogLocked returns threadID's log, creating it when absent, sweeping
// expired siblings, and refreshing the entry's deadline. Caller holds toolLogMu.
func (a *Adapter) touchToolLogLocked(threadID string) *threadToolLog {
	now := time.Now()
	if a.toolLogs == nil {
		a.toolLogs = make(map[string]ttlEntry[*threadToolLog])
	}
	sweepExpired(a.toolLogs, now)
	entry, ok := a.toolLogs[threadID]
	if !ok {
		entry = ttlEntry[*threadToolLog]{value: &threadToolLog{}}
	}
	entry.expires = now.Add(threadStateTTL)
	a.toolLogs[threadID] = entry
	return entry.value
}

// toolLogSnapshot returns a copy of threadID's retained entries and the count
// of entries the cap evicted. Empty when the thread has no live log.
func (a *Adapter) toolLogSnapshot(threadID string) (entries []toolLogEntry, dropped int) {
	a.toolLogMu.Lock()
	defer a.toolLogMu.Unlock()
	entry, ok := a.toolLogs[threadID]
	if !ok || time.Now().After(entry.expires) {
		return nil, 0
	}
	return slices.Clone(entry.value.entries), entry.value.dropped
}

// inspectNothingRetainedNotice is the ephemeral reply when the shortcut is
// invoked in a thread with no retained tool activity: no agent turn ran here,
// the log expired or was capped away, or the gateway restarted. Honest about
// the retention model rather than guessing which case applies.
const inspectNothingRetainedNotice = "_I don't have retained tool activity for this thread — either no agent turn ran here recently, or my record is gone (tool calls are kept in memory for 24 hours, don't survive a restart, and aren't recorded while `/details off` is set). For live debugging, set `/details full` in the thread before the turn runs._"

// inspectRetainedElsewhereHint extends the empty-log notice when this process
// has other traces of the thread (details setting, usage): a turn very likely
// ran, so the log was evicted rather than never written.
const inspectRetainedElsewhereHint = "_This thread has been served, so the tool log for its earlier turns is no longer retained — use `/details full` before the next turn for live debugging._"

// inspectFallbackText is the notification/accessibility fallback of an
// inspection message; the context blocks carry the real content.
const inspectFallbackText = "Agent tool activity"

// handleMessageAction routes a Slack message-shortcut invocation. Only the
// "Inspect agent steps" shortcut is registered; anything else (a stale app
// config, a forged payload) is dropped. The payload is attacker-shaped input:
// the reply is ephemeral to the invoker and thread-scoped, and the rendered
// content was escaped when it was recorded, so no field is trusted beyond
// routing.
func (a *Adapter) handleMessageAction(ctx context.Context, payload interactionPayload) {
	if payload.CallbackID != inspectShortcutCallbackID {
		return
	}
	if payload.User.ID == "" || payload.Channel.ID == "" {
		return
	}
	// The shortcut can be invoked on any message of the thread; the log is
	// keyed by the thread root (thread_ts, or the message's own ts when it is
	// a top-level message).
	threadID := payload.Message.ThreadTS
	if threadID == "" {
		threadID = payload.Message.TS
	}
	if threadID == "" {
		return
	}
	a.postInspection(ctx, payload.Channel.ID, threadID, payload.User.ID)
}

// postInspection renders threadID's retained tool log as ephemeral in-thread
// messages visible only to slackUser: per call, the detailsFull entry format
// (🔧 name + args, ↳ result preview) grouped under per-turn markers. Splits
// across several ephemeral posts when the log outgrows one message's block
// budget. An empty log gets the honest "no longer retained" guidance instead.
func (a *Adapter) postInspection(ctx context.Context, slackChannel, threadID, slackUser string) {
	client := a.apiClient()
	entries, dropped := a.toolLogSnapshot(threadID)
	if len(entries) == 0 {
		notice := inspectNothingRetainedNotice
		if a.threadEngaged(threadID) {
			notice = inspectRetainedElsewhereHint
		}
		if err := client.postEphemeralText(ctx, slackChannel, slackUser, threadID, notice); err != nil {
			a.Logger.Warn("slack: post inspection notice failed", "thread", threadID, "user", slackUser, "error", err)
		}
		return
	}

	blocks := inspectionBlocks(entries, dropped)
	for start := 0; start < len(blocks); start += maxActivityBlocks {
		end := min(start+maxActivityBlocks, len(blocks))
		if err := client.postEphemeralBlocks(ctx, slackChannel, slackUser, threadID, inspectFallbackText, blocks[start:end]); err != nil {
			a.Logger.Warn("slack: post inspection failed", "thread", threadID, "user", slackUser, "error", err)
			return
		}
	}
}

// inspectionBlocks renders retained entries as Block Kit context blocks: a
// header stating scope and visibility, a marker per turn, then one block per
// entry. Entry text is already escaped; it is re-capped to the mrkdwn element
// limit because escaping may have grown it past what was recorded.
func inspectionBlocks(entries []toolLogEntry, dropped int) []any {
	header := "🔍 *Agent tool calls in this thread* — only you can see this."
	if dropped > 0 {
		header += fmt.Sprintf(" Showing the last %d calls; %d earlier ones were dropped.", len(entries), dropped)
	}
	blocks := []any{contextBlock(header)}
	turn := 0
	for _, e := range entries {
		if e.turn != turn {
			turn = e.turn
			blocks = append(blocks, contextBlock(fmt.Sprintf("*— turn %d —*", turn)))
		}
		blocks = append(blocks, contextBlock(truncateRunes(e.md, slackSectionTextMax)))
	}
	return blocks
}

// postEphemeralBlocks posts an in-thread Block Kit message visible only to
// user (chat.postEphemeral). fallback is the notification/accessibility text.
func (c *slackAPIClient) postEphemeralBlocks(ctx context.Context, channel, user, threadTS, fallback string, blocks []any) error {
	body := map[string]any{
		paramChannel: channel,
		paramUser:    user,
		paramText:    fallback,
		paramBlocks:  blocks,
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	_, err := c.postJSON(ctx, "chat.postEphemeral", body)
	return err
}
