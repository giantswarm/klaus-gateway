package slack

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// A conversation can open inside a thread other people wrote: the "Ask an
// agent here" shortcut, an `/agent <name> <question>` reply, a bare mention
// under an alert. The agent is then pulled into a discussion it cannot see —
// which alert fired, what was already tried — so the opener's turn carries the
// messages the thread already held as a labelled part of its own.
//
// The read happens once, on the opener, and only there: every later message in
// a bound thread is a turn of the conversation already. It is NOT the history
// fallback the adapter deliberately does not have (see doc.go): routing state
// keeps its single source of truth in the thread record, and nothing read here
// is ever written back.

const (
	// threadContextMaxMessages and threadContextMaxChars cap the transcript.
	// An incident thread of a few dozen messages fits; a day-long war room
	// does not, and should not — the oldest messages go first and the root,
	// which is the alert, always stays. Constants, not configuration.
	threadContextMaxMessages = 40
	threadContextMaxChars    = 12000

	// threadContextReadTimeout bounds the whole read. Past it the turn runs
	// without the transcript rather than making the person wait for it.
	threadContextReadTimeout = 5 * time.Second

	// threadContextLabel introduces the transcript and names who shared it:
	// the messages are other people's, and the agent must never read them as
	// the initiator's own words. %s is the initiator, %d the number of earlier
	// messages, %s the ", the last N shown" note when the cap dropped some.
	threadContextLabel = "[thread context shared by %s: %d earlier %s in this thread%s, oldest first]"
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
func (a *Adapter) attachThreadContext(ctx context.Context, msg *channels.InboundMessage, slackChannel, initiator string) {
	if !msg.Opener || msg.Context != "" || msg.ThreadID == "" || msg.ThreadID == msg.MessageID {
		return
	}
	msg.Context = a.threadContext(ctx, slackChannel, msg.ThreadID, msg.MessageID, initiator)
}

// threadContext reads the messages written in threadID before openerTS and
// renders them as the opener turn's shared context. A failure — a missing
// scope, a channel the bot is not in, a slow Slack — never blocks the turn:
// it returns an empty transcript, tells the initiator once why the agent only
// sees their question, and logs the reason. No retry, no parking.
func (a *Adapter) threadContext(ctx context.Context, channelID, threadID, openerTS, initiator string) string {
	rctx, cancel := context.WithTimeout(ctx, threadContextReadTimeout)
	defer cancel()

	messages, err := a.apiClient().threadReplies(rctx, channelID, threadID)
	if err != nil {
		reason := threadContextFailureReason(rctx, err)
		a.Logger.Warn("slack: thread context read failed, the turn runs without it",
			"channel_id", channelID, "thread_id", threadID, "slack_user", initiator, "reason", reason, "error", err)
		if perr := a.apiClient().postEphemeralText(ctx, channelID, initiator, threadID, fmt.Sprintf(threadContextFailedNotice, reason)); perr != nil {
			a.Logger.Warn("slack: post thread-context notice failed", "thread_id", threadID, "error", perr)
		}
		return ""
	}
	return renderThreadContext(messages, openerTS, a.botID(ctx), a.displayName(ctx, initiator), func(userID string) string {
		return a.displayName(ctx, userID)
	})
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
// as the transcript handed to the agent, or "" when there are none. Messages
// arrive oldest first (conversations.replies order); botUserID is the
// gateway's own Slack user, whose posts are left out — the agent wrote or is
// about to write them. name resolves a Slack user ID to a display name.
func renderThreadContext(messages []threadMessage, openerTS, botUserID, initiator string, name func(string) string) string {
	var lines []string
	for _, m := range messages {
		if !earlierThan(m.TS, openerTS) || !contentfulSubtypes[m.SubType] {
			continue
		}
		if botUserID != "" && m.User == botUserID {
			continue
		}
		if line := renderThreadMessage(m, name); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	total := len(lines)
	lines = capThreadContext(lines)

	shown := ""
	if len(lines) < total {
		shown = fmt.Sprintf(", the last %d shown", len(lines))
	}
	noun := "messages"
	if total == 1 {
		noun = "message"
	}
	if initiator == "" {
		initiator = "the person who started this conversation"
	}
	return fmt.Sprintf(threadContextLabel, initiator, total, noun, shown) + "\n" + strings.Join(lines, "\n")
}

// capThreadContext drops the oldest lines until the transcript fits both caps,
// always keeping the first — the root message, which is the alert the thread
// is about. A root that is itself over the character cap is cut rather than
// dropped.
func capThreadContext(lines []string) []string {
	if len(lines) > threadContextMaxMessages {
		kept := make([]string, 0, threadContextMaxMessages)
		kept = append(kept, lines[0])
		lines = append(kept, lines[len(lines)-threadContextMaxMessages+1:]...)
	}
	for len(lines) > 1 && transcriptLen(lines) > threadContextMaxChars {
		lines = append(lines[:1:1], lines[2:]...)
	}
	if len(lines) == 1 {
		lines[0] = truncateRunes(lines[0], threadContextMaxChars)
	}
	return lines
}

// transcriptLen is the length of the joined transcript, newlines included.
func transcriptLen(lines []string) int {
	n := len(lines) - 1
	for _, l := range lines {
		n += len(l)
	}
	return n
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
	case bkSection, "header":
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
	if t := strings.TrimSpace(e.Text); t != "" {
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
