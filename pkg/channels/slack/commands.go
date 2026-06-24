package slack

import (
	"context"
	"strings"
)

const (
	cmdHelp    = "help"
	cmdStop    = "stop"
	cmdQuit    = "quit"
	cmdOpen    = "open"
	cmdLock    = "lock"
	cmdObserve = "observe"
)

// slashCommand is a parsed in-thread command.
type slashCommand struct {
	Name string   // lower-case command name, e.g. "stop", "open"
	Args []string // remaining tokens, e.g. ["<@U123456>"]
}

// parseCommand extracts a leading command from text. Both "/cmd" and "!cmd"
// are accepted: in a channel the message is mention-prefixed ("@klaus /stop"),
// so the mention is stripped first and the leading "/" survives; but in a DM
// Slack intercepts any message beginning with "/" as a native slash command
// and never delivers it to the app, so "!" is offered as an always-delivered
// alternate prefix. Returns nil when the text starts with neither.
func parseCommand(text string) *slashCommand {
	text = strings.TrimSpace(text)
	if text == "" || (text[0] != '/' && text[0] != '!') {
		return nil
	}
	parts := strings.Fields(text[1:])
	if len(parts) == 0 {
		return nil
	}
	var args []string
	if len(parts) > 1 {
		args = parts[1:]
	}
	return &slashCommand{Name: strings.ToLower(parts[0]), Args: args}
}

// isSlackUserID reports whether s looks like a Slack user/bot-user ID (U… or
// W… followed by uppercase alphanumerics). Used to reject non-ID tokens passed
// to /open so a typo can never silently broaden access.
func isSlackUserID(s string) bool {
	if len(s) < 3 || (s[0] != 'U' && s[0] != 'W') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// parseUserIDs extracts Slack user IDs from command args, accepting raw IDs
// ("U123"), "@U123", and mention tokens ("<@U123>" / "<@U123|name>"). Tokens
// that are not user IDs are skipped.
func parseUserIDs(args []string) []string {
	var ids []string
	for _, a := range args {
		a = strings.Trim(a, "<>")
		a = strings.TrimPrefix(a, "@")
		if i := strings.IndexByte(a, '|'); i >= 0 {
			a = a[:i]
		}
		if isSlackUserID(a) {
			ids = append(ids, a)
		}
	}
	return ids
}

const helpText = "*Commands*\n" +
	"In a channel, mention me first (`@klaus /stop`). In a direct message, Slack swallows `/`, so use `!` instead (`!stop`).\n" +
	"• `/stop` — interrupt the current turn\n" +
	"• `/quit` — end the session _(owner only)_\n" +
	"• `/open` — allow everyone in this thread _(owner only)_\n" +
	"• `/open @user …` — allow only the named people _(owner only)_\n" +
	"• `/lock` — restrict to owner only _(owner only)_\n" +
	"• `/observe` — toggle observe mode _(owner only)_\n" +
	"• `/help` — show this message"

// handleCommand processes a slash command and posts a reply in-thread.
// Returns true when the command was consumed (caller should not dispatch).
func (a *Adapter) handleCommand(ctx context.Context, cmd *slashCommand, slackUser, slackChannel, threadID string) bool {
	client := a.apiClient()
	reply := func(text string) {
		if _, err := client.postMessage(ctx, slackChannel, text, threadID); err != nil {
			a.Logger.Warn("slack: post command reply failed", "error", err)
		}
	}

	// ownerOnly resolves the thread's access state and verifies the caller is
	// the owner, replying with a refusal otherwise. Resolving lazily keeps the
	// non-mutating commands (/help, /stop) and unknown commands free of access
	// side effects. The first user to interact with a thread becomes its owner
	// (see getAccess).
	ownerOnly := func() (*AccessState, bool) {
		state := a.getAccess(threadID, slackUser)
		if !state.IsOwner(slackUser) {
			reply("_Only the thread owner can run this command._")
			return nil, false
		}
		return state, true
	}

	switch cmd.Name {
	case cmdHelp:
		reply(helpText)
		return true

	case cmdStop:
		a.turnsMu.Lock()
		if t, ok := a.turns[threadID]; ok {
			t.cancel()
		}
		a.turnsMu.Unlock()
		reply("⏹ Stopped.")
		return true

	case cmdQuit:
		if _, ok := ownerOnly(); !ok {
			return true
		}
		a.turnsMu.Lock()
		if t, ok := a.turns[threadID]; ok {
			t.cancel()
			delete(a.turns, threadID)
		}
		a.turnsMu.Unlock()
		a.accessMu.Lock()
		delete(a.access, threadID)
		a.accessMu.Unlock()
		reply("👋 Session ended.")
		return true

	case cmdOpen:
		state, ok := ownerOnly()
		if !ok {
			return true
		}
		switch users := parseUserIDs(cmd.Args); {
		case len(cmd.Args) == 0:
			state.Open()
			reply("🔓 Thread is now open to everyone.")
		case len(users) == 0:
			reply("_Usage:_ `/open` _(everyone) or_ `/open @user …` _(specific people)._")
		default:
			state.Allow(users...)
			reply("🔓 Now responding to the owner and the named user(s).")
		}
		return true

	case cmdLock:
		state, ok := ownerOnly()
		if !ok {
			return true
		}
		state.Lock()
		reply("🔒 Thread is now locked.")
		return true

	case cmdObserve:
		state, ok := ownerOnly()
		if !ok {
			return true
		}
		if state.ToggleObserve() {
			reply("👀 Observe mode on.")
		} else {
			reply("👀 Observe mode off.")
		}
		return true
	}

	return false
}
