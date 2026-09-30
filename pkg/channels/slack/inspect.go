package slack

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// maxToolLogEntries bounds the retained tool-call log per thread: the last N
// calls are kept, older ones are dropped (and counted, so the inspection can
// say so). Together with the threadStateTTL sweep this hard-bounds the log's
// memory on a long-lived pod. The log is in-memory only and lost on restart by
// design.
const maxToolLogEntries = 100

// toolState is where a logged call stands: running until its result arrives,
// then done, failed, or stopped at the person's approval.
type toolState int

const (
	toolRunning toolState = iota
	toolDone
	toolFailed
	toolAwaitingApproval
)

// toolLogEntry is one retained tool call and, once it arrives, its result. The
// fields are raw agent- and tool-controlled text; inspectionBlocks escapes them
// when it renders the log.
type toolLogEntry struct {
	turn   int    // 1-based turn ordinal within this thread's log
	callID string // correlates the result with the call; "" when the stream gave none
	// name is the tool that ran: call_tool's inner tool when it wrapped one, in
	// which case viaMuster is set.
	name      string
	viaMuster bool
	// called reports whether the call itself was seen; a result whose call was
	// not (a call without an id, a result after a restart) has none. args is
	// the call's arguments as indented JSON, "" for a call without any.
	called bool
	args   string
	state  toolState
	result string // the result preview; "" before it arrives, or when it was empty
}

// threadToolLog is one thread's retained tool activity. turns counts turns
// recorded since the log entry was created; dropped counts entries evicted by
// the per-thread cap, so the inspection is honest about what it shows.
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

// appendToolLog retains one entry for threadID, evicting the oldest entries
// past the per-thread cap.
func (a *Adapter) appendToolLog(threadID string, e toolLogEntry) {
	a.toolLogMu.Lock()
	defer a.toolLogMu.Unlock()
	a.appendToolLogLocked(a.touchToolLogLocked(threadID), e)
}

func (a *Adapter) appendToolLogLocked(log *threadToolLog, e toolLogEntry) {
	log.entries = append(log.entries, e)
	if over := len(log.entries) - maxToolLogEntries; over > 0 {
		log.dropped += over
		log.entries = append(log.entries[:0], log.entries[over:]...)
	}
}

// completeToolLog records a result: it closes the running call of the same
// turn it belongs to, and a result no running call matches becomes an entry of
// its own, without the call. A result belongs to the call with its call id; a
// result the stream gave no id closes the oldest running call of the same tool
// that has none either, first in, first out.
func (a *Adapter) completeToolLog(threadID string, result toolLogEntry) {
	a.toolLogMu.Lock()
	defer a.toolLogMu.Unlock()
	log := a.touchToolLogLocked(threadID)
	for i := range log.entries {
		e := &log.entries[i]
		if e.turn != result.turn || e.state != toolRunning || e.callID != result.callID {
			continue
		}
		if result.callID == "" && e.name != result.name {
			continue
		}
		e.state, e.result = result.state, result.result
		return
	}
	a.appendToolLogLocked(log, result)
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

// inspectNothingRetainedNotice is the reply when the shortcut is invoked in a
// thread with no retained tool activity: no agent turn ran here, the log
// expired or was capped away, or the gateway restarted. Honest about the
// retention model rather than guessing which case applies.
const inspectNothingRetainedNotice = "No tool activity is kept for this thread: no agent turn ran here recently, or the record is gone. Tool calls are kept in memory for 24 hours and do not survive a restart."

// inspectRetainedElsewhereHint replaces the empty-log notice when this process
// has other traces of the thread (recorded usage): a turn very likely ran, so
// the log was evicted rather than never written.
const inspectRetainedElsewhereHint = "This thread has been served, so the tool log of its earlier turns is no longer kept."

// inspectFallbackText is the notification/accessibility fallback of an
// inspection posted as a message; the blocks carry the real content.
const inspectFallbackText = "Agent tool activity"

// inspectModalTitle names the inspection modal (Slack caps a modal title at 24
// characters).
const inspectModalTitle = "Agent steps"

// maxInspectBlocks is Slack's cap on the blocks of one modal. The log keeps
// fewer calls than that, but the turn headers count too, so the oldest calls
// make room when a log of many short turns would not fit.
const maxInspectBlocks = 100

// Render budgets of one call's section, in runes of escaped text, so the
// section stays under Slack's 3 000 characters (slackSectionTextMax) however
// much the escaping grew the payload. The title is toolTitle's own cap.
const (
	inspectNameMax   = 120
	inspectArgsMax   = 1400
	inspectResultMax = 900
)

// handleMessageAction routes a Slack message-shortcut invocation by its
// callback id: "Inspect agent steps" to the tool-log modal, "Ask an agent
// here" to the agent picker. Anything else (a stale app config, a forged
// payload) is dropped. The payload is attacker-shaped input, so no field is
// trusted beyond routing: the inspection opens for the invoker alone and is
// thread-scoped, and every payload text is escaped when it renders.
func (a *Adapter) handleMessageAction(ctx context.Context, payload interactionPayload) {
	if payload.User.ID == "" || payload.Channel.ID == "" {
		return
	}
	// Either shortcut can be invoked on any message of a thread, and both key
	// on the thread root: thread_ts, or the message's own ts when it is a
	// top-level message (which the picker then opens the conversation in).
	threadID := payload.Message.ThreadTS
	if threadID == "" {
		threadID = payload.Message.TS
	}
	if threadID == "" {
		return
	}
	switch payload.CallbackID {
	case inspectShortcutCallbackID:
		a.postInspection(ctx, payload.Channel.ID, threadID, payload.User.ID, payload.TriggerID)
	case askAgentShortcutCallbackID:
		a.handleAskAgentShortcut(ctx, payload, threadID)
	}
}

// postInspection shows threadID's retained tool log to slackUser in a modal
// opened with the shortcut's trigger_id. Should Slack refuse the modal — the
// trigger_id lives only 3 seconds — the same content goes out as ephemeral
// in-thread messages instead, split across several when it outgrows one
// message's block budget. An empty log gets the honest "no longer retained"
// guidance instead.
func (a *Adapter) postInspection(ctx context.Context, slackChannel, threadID, slackUser, triggerID string) {
	client := a.apiClient()
	entries, dropped := a.toolLogSnapshot(threadID)
	notice := ""
	var blocks []any
	if len(entries) == 0 {
		notice = inspectNothingRetainedNotice
		if a.threadEngaged(threadID) {
			notice = inspectRetainedElsewhereHint
		}
		blocks = []any{sectionBlock(notice)}
	} else {
		blocks = inspectionBlocks(entries, dropped)
	}

	if triggerID != "" {
		err := client.viewsOpen(ctx, triggerID, inspectionView(blocks))
		if err == nil {
			return
		}
		a.Logger.Warn("slack: open the inspection modal failed, posting it in the thread instead", "thread", threadID, "user", slackUser, "error", err)
	}

	if notice != "" {
		if err := client.postEphemeralText(ctx, slackChannel, slackUser, threadID, notice); err != nil {
			a.Logger.Warn("slack: post inspection notice failed", "thread", threadID, "user", slackUser, "error", err)
		}
		return
	}
	for start := 0; start < len(blocks); start += maxActivityBlocks {
		end := min(start+maxActivityBlocks, len(blocks))
		if err := client.postEphemeralBlocks(ctx, slackChannel, slackUser, threadID, inspectFallbackText, blocks[start:end]); err != nil {
			a.Logger.Warn("slack: post inspection failed", "thread", threadID, "user", slackUser, "error", err)
			return
		}
	}
}

// inspectionView is the modal that shows the inspection blocks.
func inspectionView(blocks []any) map[string]any {
	return map[string]any{
		bkType:   bkModal,
		bkTitle:  plainTextObj(inspectModalTitle),
		bkClose:  plainTextObj("Close"),
		bkBlocks: blocks,
	}
}

// sectionBlock wraps mrkdwn in a Block Kit section block.
func sectionBlock(md string) map[string]any {
	return map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: md}}
}

// headerBlock is a Block Kit header block of plain text.
func headerBlock(text string) map[string]any {
	return map[string]any{bkType: bkHeader, bkText: plainTextObj(text)}
}

// inspectionBlocks renders retained entries as Block Kit blocks: a context
// line stating scope and visibility, then per turn a divider and a header, and
// one section per call. The oldest calls give way when the log would outgrow a
// modal, and the context line counts them with the ones the cap dropped.
func inspectionBlocks(entries []toolLogEntry, dropped int) []any {
	for len(entries) > 0 && 1+len(entries)+2*countTurns(entries) > maxInspectBlocks {
		entries = entries[1:]
		dropped++
	}
	scope := fmt.Sprintf("Visible only to you · the last %d calls of this thread, kept for 24 hours", maxToolLogEntries)
	if dropped > 0 {
		scope += fmt.Sprintf(" · %d earlier %s not shown", dropped, plural(dropped, "call is", "calls are"))
	}
	blocks := []any{contextBlock(scope)}
	for i := 0; i < len(entries); {
		turn := entries[i].turn
		j := i
		for j < len(entries) && entries[j].turn == turn {
			j++
		}
		header := fmt.Sprintf("Turn %d · %d %s", turn, j-i, plural(j-i, "call", "calls"))
		blocks = append(blocks, map[string]any{bkType: bkDivider}, headerBlock(header))
		for _, e := range entries[i:j] {
			blocks = append(blocks, sectionBlock(callSectionText(e)))
		}
		i = j
	}
	return blocks
}

// countTurns counts the runs of one turn in entries, which is how many turn
// headers the inspection renders.
func countTurns(entries []toolLogEntry) int {
	n, turn := 0, -1
	for _, e := range entries {
		if e.turn != turn {
			n, turn = n+1, e.turn
		}
	}
	return n
}

// callSectionText renders one call: its status, its plain-language title with
// the raw tool name, its arguments and its result. Every payload is agent- or
// tool-controlled, so it is escaped, kept inside its code block and cut to its
// share of the section's size.
func callSectionText(e toolLogEntry) string {
	var b strings.Builder
	b.WriteString(toolStateIcon(e.state) + " *" + strings.ReplaceAll(toolTitle(e.name), "`", "'") + "*  ·  `" +
		codeSpanSafe(cutEscaped(escapeMrkdwn(e.name), inspectNameMax)) + "`")
	if e.viaMuster {
		b.WriteString("  via muster")
	}
	switch {
	case !e.called:
		b.WriteString("\n_The call itself was not recorded._")
	case e.args == "":
		b.WriteString("\n_No arguments._")
	default:
		b.WriteString("\n*Arguments*\n" + codeBlock(e.args, inspectArgsMax))
	}
	switch e.state {
	case toolRunning:
		b.WriteString("\n_No result yet._")
	case toolAwaitingApproval:
		b.WriteString("\n_Asked for approval. If it was approved, the call and its result are in a later turn._")
	default:
		label := "*Result*"
		if e.state == toolFailed {
			label = "*Error*"
		}
		if e.result == "" {
			b.WriteString("\n" + label + ": _no output._")
		} else {
			b.WriteString("\n" + label + "\n" + codeBlock(e.result, inspectResultMax))
		}
	}
	return b.String()
}

// toolStateIcon is the status mark a call's section opens with.
func toolStateIcon(s toolState) string {
	switch s {
	case toolDone:
		return "✅"
	case toolFailed:
		return "❌"
	case toolAwaitingApproval:
		return "⏸"
	}
	return "⏳"
}

// codeBlock renders untrusted text as an mrkdwn code block: escaped, cut to max
// runes, and with its backticks replaced so the text cannot close the block.
func codeBlock(s string, max int) string {
	return "```" + strings.ReplaceAll(cutEscaped(escapeMrkdwn(s), max), "`", "'") + "```"
}

// cutEscaped caps escaped text at max runes like truncateRunes, but never
// leaves a partial entity at the cut: every "&" of escaped text opens one of
// &amp;, &lt; or &gt;, so a cut that lands inside one backs off to the "&".
func cutEscaped(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	kept := r[:max-1]
	// An entity is at most 5 runes, so an unterminated "&" can only sit within
	// the last 4 kept runes; meeting a ";" first means the last one is whole.
	for i := len(kept) - 1; i >= 0 && i >= len(kept)-4; i-- {
		if kept[i] == ';' {
			break
		}
		if kept[i] == '&' {
			kept = kept[:i]
			break
		}
	}
	return string(kept) + "…"
}

// plural picks the singular or the plural form for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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
