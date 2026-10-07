package slack

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// A conversation can open inside a thread other people wrote: the "Ask an
// agent here" shortcut, a pick in the agent picker, a bare mention
// under an alert. The agent is then pulled into a discussion it cannot see —
// which alert fired, what was already tried — so the opener's turn carries the
// messages the thread already held as a labelled part of its own.
//
// The read happens once, on the opener: every later message in a bound thread
// is a turn of the conversation already. The one other read is the catch-up
// of a muted thread, on the turn that ends the mute, of the messages the gate
// dropped while it lasted (see mute.go). It is NOT the history
// fallback the adapter deliberately does not have (see doc.go): routing state
// keeps its single source of truth in the thread record, and nothing read here
// is ever written back.

const (
	// threadContextMaxChars caps the transcript. An incident thread of a few
	// dozen messages fits; a day-long war room does not, and should not — the
	// oldest messages go first and the root, which is the alert, always stays.
	// Characters, counted as characters and not as bytes, so an accented
	// transcript is not cut a third of the way early. A constant, not
	// configuration.
	threadContextMaxChars = 12000

	// threadContextReadTimeout bounds every call the transcript costs: the
	// paged thread read AND the display-name lookups behind it. Past it the
	// turn runs with what was rendered — authors named by their ID, or no
	// transcript at all — rather than making the person wait for it.
	threadContextReadTimeout = 5 * time.Second

	// threadContextLabel introduces the transcript and names who shared it:
	// the messages are other people's, and the agent must never read them as
	// the initiator's own words. %s are the initiator, what was shared, and
	// how it was shortened.
	threadContextLabel = "[thread context shared by %s: %s%s]"

	// threadContextRead names a complete read, threadContextPartial one the
	// page bound stopped before the end of the thread — with the thread's own
	// message count when the root reported one, and without it when it did
	// not. A partial read must never read as "the latest N messages": its
	// lines are neither the oldest nor the newest, and an agent told otherwise
	// would take stale messages for the state of play.
	threadContextRead         = "%s earlier %s in this thread"
	threadContextPartial      = "%s of %s earlier messages read (the read stopped early)"
	threadContextPartialShort = "%s earlier messages read (the read stopped early)"

	// threadContextOldestFirst ends a label whose transcript is whole;
	// threadContextTrimmed ends one the character cap shortened.
	threadContextOldestFirst = ", oldest first"
	threadContextTrimmed     = ", the most recent %s characters shown"

	// catchUpLabel introduces what the people wrote in a muted thread, handed
	// to the turn that ends the mute. %s are what was shared and how it was
	// shortened. Its own label: the lines are not the thread's opening, and
	// the agent must not read them as the asker's words either.
	catchUpLabel = "[messages written in this thread while the agent was muted: %s%s]"

	// catchUpRead counts the messages shared; catchUpPartial follows it when
	// the page bound or Slack stopped the read. A catch-up read starts at the
	// mute, so the thread's own reply count says nothing about it, and the
	// lines are the oldest of the muted period, not its newest.
	catchUpRead    = "%s %s"
	catchUpPartial = " (the read stopped early)"
)

// mentionRe matches a Slack user mention, with or without the "|label" form
// older clients still send.
var mentionRe = regexp.MustCompile(`<@([UWB][A-Z0-9]+)(\|[^>]*)?>`)

// contentfulSubtypes are the message subtypes that carry words a reader wants.
// Everything else Slack marks with a subtype is a channel event (a join, a
// topic change, a pane anchor) or an edit envelope, and is skipped.
var contentfulSubtypes = map[string]bool{
	"":                 true,
	"bot_message":      true,
	"file_share":       true,
	"thread_broadcast": true,
	"me_message":       true,
}

// attachThreadContext gives msg the transcript of what its thread already said,
// when msg opens a conversation inside a thread that existed before it. A
// message that roots its own thread has nothing earlier to read, and a turn in
// a conversation that is already running is not an opener.
//
// A DM is skipped whatever its thread looks like: the assistant pane roots
// every chat at an anchor Slack creates when the chat opens, so the opener is
// never its own thread root there and the read would only ever return the
// person's own words back to them — one Slack call per new chat for nothing.
func (a *Adapter) attachThreadContext(ctx context.Context, msg *channels.InboundMessage, slackChannel, initiator string) {
	if !msg.Opener || msg.Context != "" || msg.ThreadID == "" || msg.ThreadID == msg.MessageID {
		return
	}
	if isDMChannelID(slackChannel) {
		return
	}
	msg.Context = a.threadContext(ctx, slackChannel, msg.ThreadID, msg.MessageID, initiator)
}

// threadContext reads the messages written in threadID before openerTS and
// renders them as the opener turn's shared context.
func (a *Adapter) threadContext(ctx context.Context, channelID, threadID, openerTS, initiator string) string {
	return a.readThreadFor(ctx, threadReadSpec{
		what: "thread context", channelID: channelID, threadID: threadID, asker: initiator,
		failedNotice: threadContextFailedNotice,
		render: func(read threadRead, botUserID string, name func(string) string) string {
			return renderThreadContext(read, openerTS, botUserID, name(initiator), name)
		},
	})
}

// attachCatchUp gives msg, the message that ended its thread's mute, what the
// people wrote in the thread while it was muted: the messages after mutedAt
// and before msg. A message that already carries context (a shortcut's shared
// thread) holds those messages already, and a mute with nothing written
// after it leaves the turn with its own message only.
func (a *Adapter) attachCatchUp(ctx context.Context, msg *channels.InboundMessage, slackChannel, asker, mutedAt string) {
	if mutedAt == "" || msg.Context != "" {
		return
	}
	msg.Context = a.readThreadFor(ctx, threadReadSpec{
		what: "catch-up", channelID: slackChannel, threadID: msg.ThreadID, oldest: mutedAt, asker: asker,
		failedNotice: catchUpFailedNotice,
		render: func(read threadRead, botUserID string, name func(string) string) string {
			return renderCatchUp(read, msg.ThreadID, mutedAt, msg.MessageID, botUserID, name)
		},
	})
}

// threadReadSpec is one read of a thread's messages for a turn: what the log
// calls it, the thread, the ts the read starts after ("" for the whole
// thread), the person told when it fails and the notice they get, and how
// the read becomes the turn's context.
type threadReadSpec struct {
	what                string
	channelID, threadID string
	oldest, asker       string
	failedNotice        string
	render              func(read threadRead, botUserID string, name func(string) string) string
}

// readThreadFor reads a thread and renders it for a turn. A failure — a
// missing scope, a channel the bot is not in, a slow Slack — never blocks the
// turn: it returns an empty transcript, tells the asker once why the agent
// only sees their message, and logs the reason. No retry, no parking.
func (a *Adapter) readThreadFor(ctx context.Context, spec threadReadSpec) string {
	channelID, threadID, initiator := spec.channelID, spec.threadID, spec.asker
	rctx, cancel := context.WithTimeout(ctx, threadContextReadTimeout)
	defer cancel()

	// Every lookup the transcript needs runs on rctx, not on the turn's own
	// context: the thread read, this one, and the name of each author. Slack's
	// 429 wait honours the context it was given, so a rate-limited users.info
	// cannot hold the first reply past the budget — past it an author is named
	// by their ID, which is the fallback anyway.
	botUserID := a.botID(rctx)
	read, err := a.apiClient().threadReplies(rctx, channelID, threadID, spec.oldest)
	if err != nil {
		reason := threadContextFailureReason(rctx, err)
		a.Logger.Warn("slack: "+spec.what+" read failed, the turn runs without it",
			"channel_id", channelID, "thread_id", threadID, "slack_user", initiator, "reason", reason, "error", err)
		if perr := a.apiClient().postEphemeralText(ctx, channelID, initiator, threadID, fmt.Sprintf(spec.failedNotice, reason)); perr != nil {
			a.Logger.Warn("slack: post "+spec.what+" notice failed", "thread_id", threadID, "error", perr)
		}
		return ""
	}
	if read.Err != nil {
		// The read has messages, so the turn gets a partial transcript and the
		// label says so — but an operator must be able to tell a read the page
		// bound stopped from one Slack cut short.
		a.Logger.Warn("slack: "+spec.what+" read stopped early, the turn gets part of the thread",
			"channel_id", channelID, "thread_id", threadID, "slack_user", initiator,
			"reason", threadContextFailureReason(rctx, read.Err), "messages", len(read.Messages), "error", read.Err)
	}
	name := func(userID string) string { return a.displayName(rctx, userID) }
	transcript := spec.render(read, botUserID, name)
	if transcript != "" {
		// The transcript is never posted anywhere a person sees it, so this
		// record is the only trace that the turn carried the thread. lines
		// is what the agent was given (one per message shared, after the
		// skips and the cap), not the number of messages the read returned.
		a.Logger.Info("slack: "+spec.what+" attached to the turn",
			"channel_id", channelID, "thread_id", threadID, "slack_user", initiator,
			"lines", strings.Count(transcript, "\n"), "complete", read.Complete, "chars", utf8.RuneCountInString(transcript))
	}
	return transcript
}

// threadContextFailureReason names the failure the way the notice and the log
// report it: Slack's own error code, or the timeout that ran out first.
func threadContextFailureReason(ctx context.Context, err error) string {
	if code := apiErrorCode(err); code != "" {
		return code
	}
	if ctx.Err() != nil {
		return "timeout"
	}
	return "unavailable"
}

// renderThreadContext renders the messages of a thread that precede openerTS
// as the transcript handed to the agent, or "" when there are none. The read
// holds them oldest first (conversations.replies order); botUserID is the
// gateway's own Slack user, whose posts are left out — the agent wrote or is
// about to write them. name resolves a Slack user ID to a display name.
func renderThreadContext(read threadRead, openerTS, botUserID, initiator string, name func(string) string) string {
	lines := renderThreadLines(read, botUserID, name, func(m threadMessage) bool { return earlierThan(m.TS, openerTS) })
	if len(lines) == 0 {
		return ""
	}
	total := len(lines)
	lines = capThreadContext(lines)
	if initiator == "" {
		initiator = "the person who started this conversation"
	}

	shortened := threadContextOldestFirst
	if len(lines) < total {
		shortened = fmt.Sprintf(threadContextTrimmed, thousands(threadContextMaxChars))
	}
	return fmt.Sprintf(threadContextLabel, initiator, threadContextShared(read, total), shortened) +
		"\n" + strings.Join(lines, "\n")
}

// threadContextShared says how much of the thread the transcript stands for: a
// count of what the thread holds when the read reached its end, and how far
// the read got out of how long the thread is when it did not.
func threadContextShared(read threadRead, total int) string {
	if !read.Complete {
		if read.Total > 0 {
			return fmt.Sprintf(threadContextPartial, thousands(len(read.Messages)), thousands(read.Total))
		}
		return fmt.Sprintf(threadContextPartialShort, thousands(len(read.Messages)))
	}
	noun := "messages"
	if total == 1 {
		noun = "message"
	}
	return fmt.Sprintf(threadContextRead, thousands(total), noun)
}

// thousands renders a count with thousands separators, so a four-digit message
// count in the label reads at a glance.
func thousands(n int) string {
	digits := strconv.Itoa(n)
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return b.String()
}

// capThreadContext keeps the root and the newest lines that fit the character
// cap under it, dropping the oldest — the root is the alert the thread is
// about, and stays whatever it costs; a root that is itself over the cap is
// cut rather than dropped. The lines are measured once, from the newest
// backwards, because a thread read can bring several hundred of them.
func capThreadContext(lines []string) []string {
	if len(lines) == 1 {
		lines[0] = truncateRunes(lines[0], threadContextMaxChars)
		return lines
	}
	// The joined transcript costs the root plus, for every other line, its own
	// length and the newline before it.
	rest := newestWithin(lines[1:], threadContextMaxChars-utf8.RuneCountInString(lines[0]))
	if len(rest) == 0 {
		return []string{truncateRunes(lines[0], threadContextMaxChars)}
	}
	return append(lines[:1:1], rest...)
}

// newestWithin keeps the newest lines whose lengths, each with one newline,
// fit budget, measured from the newest backwards.
func newestWithin(lines []string, budget int) []string {
	first := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		cost := utf8.RuneCountInString(lines[i]) + 1
		if cost > budget {
			break
		}
		budget -= cost
		first = i
	}
	return lines[first:]
}

// renderCatchUp renders the messages of a muted thread written after mutedAt
// and before untilTS — the message that ended the mute — as the catch-up
// handed to the agent, or "" when there are none. The thread's root is left
// out by its ts, whether or not Slack returns it for a later oldest, and
// nothing is pinned: the newest lines are kept under the character cap, and
// a newest line over the cap on its own is cut rather than dropped.
func renderCatchUp(read threadRead, rootTS, mutedAt, untilTS, botUserID string, name func(string) string) string {
	lines := renderThreadLines(read, botUserID, name, func(m threadMessage) bool {
		return m.TS != rootTS && tsAfter(m.TS, mutedAt) && tsAfter(untilTS, m.TS)
	})
	if len(lines) == 0 {
		return ""
	}
	total := len(lines)
	// The joined lines cost one newline fewer than they number.
	kept := newestWithin(lines, threadContextMaxChars+1)
	cut := len(kept) == 0
	if cut {
		kept = []string{truncateRunes(lines[total-1], threadContextMaxChars)}
	}

	noun := "messages"
	if total == 1 {
		noun = "message"
	}
	shared := fmt.Sprintf(catchUpRead, thousands(total), noun)
	if !read.Complete {
		shared += catchUpPartial
	}
	shortened := threadContextOldestFirst
	if cut || len(kept) < total {
		shortened = fmt.Sprintf(threadContextTrimmed, thousands(threadContextMaxChars))
	}
	return fmt.Sprintf(catchUpLabel, shared, shortened) + "\n" + strings.Join(kept, "\n")
}

// renderThreadLines renders the messages of a read that keep passes, one line
// each, oldest first. Channel events, the gateway's own posts (the agent
// wrote them) and messages with no words are left out.
func renderThreadLines(read threadRead, botUserID string, name func(string) string, keep func(threadMessage) bool) []string {
	var lines []string
	for _, m := range read.Messages {
		if !keep(m) || !contentfulSubtypes[m.SubType] {
			continue
		}
		if botUserID != "" && m.User == botUserID {
			continue
		}
		if line := renderThreadMessage(m, name); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// renderThreadMessage renders one message as "YYYY-MM-DD HH:MM author: text"
// (UTC), with every further line of its body indented two spaces so a
// multi-line message reads as one entry. Returns "" for a message with no
// words at all.
func renderThreadMessage(m threadMessage, name func(string) string) string {
	body := replaceMentions(strings.TrimSpace(m.Text), name)
	for _, extra := range messageExtras(m) {
		if strings.Contains(body, extra) {
			continue
		}
		body = strings.TrimSpace(body + "\n" + replaceMentions(extra, name))
	}
	if body == "" {
		return ""
	}
	head := messageTime(m.TS) + " " + messageAuthor(m, name) + ": "
	lines := strings.Split(body, "\n")
	out := head + lines[0]
	for _, l := range lines[1:] {
		out += "\n  " + l
	}
	return out
}

// messageExtras is everything a message says outside its plain text: the
// attachments a bot integration fills instead of the text (which is what makes
// a PagerDuty alert readable), the Block Kit layout of an app post, and the
// names of the files shared with it. Files are named, never downloaded.
func messageExtras(m threadMessage) []string {
	var out []string
	for _, att := range m.Attachments {
		for _, part := range []string{att.Title, att.Text} {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		for _, f := range att.Fields {
			if title, value := strings.TrimSpace(f.Title), strings.TrimSpace(f.Value); title != "" || value != "" {
				out = append(out, strings.TrimSpace(title+": "+value))
			}
		}
	}
	for _, b := range m.Blocks {
		out = append(out, blockText(b)...)
	}
	for _, f := range m.Files {
		if f.Name != "" {
			out = append(out, "[file: "+f.Name+"]")
		}
	}
	return out
}

// blockText is the words of one Block Kit block: a section's text and fields,
// a header's text, and the elements of a context or rich_text block. Other
// block types (dividers, images, actions) carry no prose worth transcribing.
func blockText(b threadBlock) []string {
	var out []string
	switch b.Type {
	case bkSection, bkHeader:
		if b.Text != nil {
			if t := strings.TrimSpace(b.Text.Text); t != "" {
				out = append(out, t)
			}
		}
		for _, f := range b.Fields {
			if t := strings.TrimSpace(f.Text); t != "" {
				out = append(out, t)
			}
		}
	case bkContext, "rich_text":
		for _, e := range b.Elements {
			out = append(out, elementText(e)...)
		}
	}
	return out
}

// elementText is the text of a block element and of the elements nested in it
// (a rich_text block wraps its runs in sections).
func elementText(e threadBlockElem) []string {
	var out []string
	if t := strings.TrimSpace(string(e.Text)); t != "" {
		out = append(out, t)
	}
	for _, nested := range e.Elements {
		out = append(out, elementText(nested)...)
	}
	return out
}

// messageAuthor names who wrote a message: the display name of a human author,
// or — for an integration's post — the name it posted under, since a bot has
// no profile to look up.
func messageAuthor(m threadMessage, name func(string) string) string {
	if m.BotID != "" {
		if m.Username != "" {
			return m.Username
		}
		if m.BotProfile.Name != "" {
			return m.BotProfile.Name
		}
	}
	if m.User != "" {
		return name(m.User)
	}
	if m.Username != "" {
		return m.Username
	}
	return "unknown"
}

// messageTime renders a Slack ts as a short UTC timestamp. An unparsable ts
// (never seen from Slack) renders as the raw value, which is still ordered.
func messageTime(ts string) string {
	secs, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return ts
	}
	return time.Unix(n, 0).UTC().Format("2006-01-02 15:04")
}

// earlierThan compares two Slack ts values, which are decimal seconds and
// therefore ordered as numbers. An empty opener ts admits everything.
func earlierThan(ts, openerTS string) bool {
	if ts == "" {
		return false
	}
	if openerTS == "" {
		return true
	}
	return tsValue(ts) < tsValue(openerTS)
}

func tsValue(ts string) float64 {
	v, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return 0
	}
	return v
}

// replaceMentions swaps every <@U…> for the user's display name, so a
// transcript reads as prose instead of as opaque IDs. Channel links and URLs
// are left as Slack wrote them.
func replaceMentions(text string, name func(string) string) string {
	return mentionRe.ReplaceAllStringFunc(text, func(m string) string {
		groups := mentionRe.FindStringSubmatch(m)
		return name(groups[1])
	})
}

// displayName resolves a Slack user ID to the name people see, memoized for
// userDisplayNameCacheTTL. users.info is Tier-4 rate-limited and a transcript
// names a handful of authors, so the cache keeps a thread read to one lookup
// per person. A failed or empty lookup falls back to the ID, which still
// identifies the author to anyone reading the thread.
func (a *Adapter) displayName(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	now := time.Now()
	a.displayNameMu.Lock()
	if e, ok := a.displayNames[userID]; ok && now.Before(e.expires) {
		a.displayNameMu.Unlock()
		return e.value
	}
	a.displayNameMu.Unlock()

	// Past the read's budget every author this process does not already know
	// is named by their ID: the fallback is already the contract, and one more
	// rate-limited users.info would hold the turn for a line nobody is waiting
	// for. A name already in hand costs nothing, so it is used either way.
	if ctx.Err() != nil {
		return userID
	}

	name, err := a.apiClient().lookupUserDisplayName(ctx, userID)
	if err != nil {
		a.Logger.Info("slack: display-name lookup failed, using the user ID", "slack_user", userID, "error", err)
		return userID
	}
	if name == "" {
		name = userID
	}
	a.displayNameMu.Lock()
	if a.displayNames == nil {
		a.displayNames = make(map[string]ttlEntry[string])
	}
	a.displayNames[userID] = ttlEntry[string]{value: name, expires: now.Add(userDisplayNameCacheTTL)}
	a.displayNameMu.Unlock()
	return name
}

// userDisplayNameCacheTTL bounds how long a resolved display name is reused.
// Names change rarely and a stale one only mislabels a transcript line, so the
// TTL is long enough to keep users.info off the read path.
const userDisplayNameCacheTTL = time.Hour
