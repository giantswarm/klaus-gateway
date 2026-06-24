package slack

import (
	"context"
	"strings"
)

const (
	cmdHelp   = "help"
	cmdStop   = "stop"
	cmdQuit   = "quit"
	cmdInvite = "invite"
	cmdLock   = "lock"
)

// slashCommand is a parsed in-thread command.
type slashCommand struct {
	Name string   // lower-case command name, e.g. "stop", "open"
	Args []string // remaining tokens, e.g. ["<@U123456>"]
}

// parseCommand extracts a leading /command from text. Commands are always
// mention-prefixed in use ("@klaus /stop"); StripMention removes the mention
// before this runs, leaving the leading "/" intact. Addressing the bot also
// keeps Slack from intercepting the message as a native slash command, so the
// same form works in channels and DMs. Returns nil when the text does not
// start with a slash.
func parseCommand(text string) *slashCommand {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
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

// parseUserIDs extracts the user IDs from "<@U123>" mention tokens in the
// command args. Slack encodes a user mention in message text as "<@USERID>";
// any token that is not in that form is ignored.
func parseUserIDs(args []string) []string {
	var ids []string
	for _, a := range args {
		id, ok := strings.CutPrefix(a, "<@")
		if !ok {
			continue
		}
		if id, ok := strings.CutSuffix(id, ">"); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

const helpText = "*Commands* — mention me first, e.g. `@klaus /stop`.\n" +
	"• `/stop` — interrupt the current turn\n" +
	"• `/quit` — end the session _(owner only)_\n" +
	"• `/invite @user` — let the mentioned people join this thread _(owner only)_\n" +
	"• `/lock` — restrict to the owner only _(owner only)_\n" +
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

	case cmdInvite:
		state, ok := ownerOnly()
		if !ok {
			return true
		}
		users := parseUserIDs(cmd.Args)
		if len(users) == 0 {
			reply("_Usage:_ `/invite @user` — _mention the people to add to this thread._")
			return true
		}
		state.Invite(users...)
		reply("✅ Invited the mentioned user(s) to this thread.")
		return true

	case cmdLock:
		state, ok := ownerOnly()
		if !ok {
			return true
		}
		state.Lock()
		reply("🔒 Thread is now locked.")
		return true
	}

	return false
}
