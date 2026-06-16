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

// slashCommand is a parsed in-thread slash command.
type slashCommand struct {
	Name string   // lower-case command name, e.g. "stop", "open"
	Args []string // remaining tokens, e.g. ["@U123456"]
}

// parseCommand extracts a leading /command from text.
// Returns nil when the text does not start with a slash.
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

const helpText = `*Commands*
• ` + "`/stop`" + ` — interrupt the current turn
• ` + "`/quit`" + ` — end the session _(owner only)_
• ` + "`/open`" + ` — allow everyone in this thread _(owner only)_
• ` + "`/lock`" + ` — restrict to owner only _(owner only)_
• ` + "`/observe`" + ` — toggle observe mode _(owner only)_
• ` + "`/help`" + ` — show this message`

// handleCommand processes a slash command and posts a reply in-thread.
// Returns true when the command was consumed (caller should not dispatch).
func (a *Adapter) handleCommand(ctx context.Context, cmd *slashCommand, slackUser, slackChannel, threadID string) bool {
	client := a.apiClient()
	reply := func(text string) {
		if _, err := client.postMessage(ctx, slackChannel, text, threadID); err != nil {
			a.Logger.Warn("slack: post command reply failed", "error", err)
		}
	}

	state := a.getAccess(threadID, slackUser)
	isOwner := state.IsOwner(slackUser)

	switch cmd.Name {
	case cmdHelp:
		reply(helpText)
		return true

	case cmdStop:
		a.turnsMu.Lock()
		if cancel, ok := a.turns[threadID]; ok {
			cancel()
		}
		a.turnsMu.Unlock()
		reply("⏹ Stopped.")
		return true

	case cmdQuit:
		if !isOwner {
			reply("_Only the thread owner can end the session._")
			return true
		}
		a.turnsMu.Lock()
		if cancel, ok := a.turns[threadID]; ok {
			cancel()
			delete(a.turns, threadID)
		}
		a.turnsMu.Unlock()
		a.accessMu.Lock()
		delete(a.access, threadID)
		a.accessMu.Unlock()
		reply("👋 Session ended.")
		return true

	case cmdOpen:
		if !isOwner {
			reply("_Only the thread owner can change access._")
			return true
		}
		state.Open()
		reply("🔓 Thread is now open.")
		return true

	case cmdLock:
		if !isOwner {
			reply("_Only the thread owner can change access._")
			return true
		}
		state.Lock()
		reply("🔒 Thread is now locked.")
		return true

	case cmdObserve:
		if !isOwner {
			reply("_Only the thread owner can change access._")
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
