package slack

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

const (
	cmdHelp    = "help"
	cmdStop    = "stop"
	cmdLogin   = "login"  // OBO account linking: sign in
	cmdLogout  = "logout" // OBO account linking: sign out
	cmdUsage   = "usage"
	cmdDetails = "details"
)

// detailsLevel controls how much of the agent's tool activity is rendered
// inline in a thread. The zero value is detailsOn so an un-set thread defaults
// to showing tool calls (the MVP default).
type detailsLevel int

const (
	detailsOn   detailsLevel = iota // show tool calls compactly (default)
	detailsOff                      // hide tool activity
	detailsFull                     // show tool calls and result previews
)

const (
	argOn   = "on"
	argOff  = "off"
	argFull = "full"
)

func (l detailsLevel) String() string {
	switch l {
	case detailsOff:
		return argOff
	case detailsFull:
		return argFull
	default:
		return argOn
	}
}

// parseDetailsLevel maps a command argument to a level. ok is false for an
// unrecognised argument.
func parseDetailsLevel(s string) (level detailsLevel, ok bool) {
	switch strings.ToLower(s) {
	case argOn:
		return detailsOn, true
	case argOff:
		return detailsOff, true
	case argFull:
		return detailsFull, true
	default:
		return detailsOn, false
	}
}

// knownCommands is the verb set handleCommand owns.
var knownCommands = map[string]struct{}{
	cmdHelp:    {},
	cmdStop:    {},
	cmdLogin:   {},
	cmdLogout:  {},
	cmdUsage:   {},
	cmdDetails: {},
}

// commandShapeRe matches a verb that reads as a command word. A path or URL
// fragment ("/etc/hosts", "/api/v1/pods") contains characters outside it, so a
// real prompt that happens to start with "/" still reaches the agent.
var commandShapeRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// isUnknownCommand reports whether cmd carries a command-shaped verb the
// gateway does not own (a Slack built-in like /invite, or a typo). Dispatching
// such a message burns a full agent turn on explaining slash commands.
func isUnknownCommand(cmd *slashCommand) bool {
	if _, ok := knownCommands[cmd.Name]; ok {
		return false
	}
	return commandShapeRe.MatchString(cmd.Name)
}

// slashCommand is a parsed in-thread command.
type slashCommand struct {
	Name string   // lower-case command name, e.g. "stop", "usage"
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

const helpText = "*Commands* — mention me first, e.g. `@klaus /stop`.\n" +
	"• `/stop` — interrupt the current turn\n" +
	"• `/usage` — show token usage for the last turn and the session\n" +
	"• `/details on|off|full` — show or hide the agent's tool activity\n" +
	"• `/help` — show this message"

// oboHelpText is appended to helpText when OBO account linking is enabled.
const oboHelpText = `
• ` + "`/login`" + ` — sign in to Giant Swarm so I act as you
• ` + "`/logout`" + ` — sign out`

// handleCommand processes a slash command and posts a reply in-thread.
// Returns true when the command was consumed (caller should not dispatch).
func (a *Adapter) handleCommand(ctx context.Context, cmd *slashCommand, slackUser, slackChannel, threadID string) bool {
	client := a.apiClient()
	reply := func(text string) {
		if _, err := client.postMessage(ctx, slackChannel, text, threadID); err != nil {
			a.Logger.Warn("slack: post command reply failed", "error", err)
		}
	}
	// Sign-in state is caller-only information; a shared thread must not see
	// the linked email, so /login and /logout confirm ephemerally.
	ephemeralReply := func(text string) {
		if err := client.postEphemeralText(ctx, slackChannel, slackUser, threadID, text); err != nil {
			a.Logger.Warn("slack: post ephemeral command reply failed", "error", err)
		}
	}

	// permittedOnly verifies the caller may instruct the agent in this thread
	// (the initiator, or a user the initiator granted), replying with a refusal
	// otherwise. It gates the state-changing / info commands (#124) so a pure
	// onlooker cannot flip thread-wide verbosity, read usage, or cancel a turn.
	// SetInitiator makes the first caller of any interaction the initiator, the
	// same first-sight rule dispatch uses.
	permittedOnly := func() bool {
		access := a.accessPolicy()
		access.SetInitiator(threadID, slackUser)
		if !access.Allowed(threadID, slackUser) {
			reply("_You can read this thread, but only people the thread owner has allowed can instruct the agent (that includes this command). Post a message and the owner can let you in._")
			return false
		}
		return true
	}

	switch cmd.Name {
	case cmdHelp:
		text := helpText
		if a.OBO != nil {
			text += oboHelpText
		}
		reply(text)
		return true

	case cmdLogin:
		return a.handleLoginCommand(ctx, slackUser, slackChannel, threadID, ephemeralReply)

	case cmdLogout:
		return a.handleLogoutCommand(slackUser, ephemeralReply)

	case cmdStop:
		if !permittedOnly() {
			return true
		}
		a.turnsMu.Lock()
		t, running := a.turns[threadID]
		if running {
			t.cancel()
		}
		a.turnsMu.Unlock()
		// A thread paused on input-required has no in-flight turn to cancel; the
		// paused task must be resolved with a rejection or the tool call dangles.
		// Falling through to dispatch routes "/stop" like a typed "stop" reply,
		// which decisionFromText maps to a structured reject.
		if !running && a.hasPendingTask(threadID) {
			return false
		}
		reply("⏹ Stopped.")
		return true

	case cmdUsage:
		if !permittedOnly() {
			return true
		}
		reply(a.usageReport(ctx, threadID, slackChannel))
		return true

	case cmdDetails:
		if !permittedOnly() {
			return true
		}
		if len(cmd.Args) == 0 {
			reply(fmt.Sprintf("Tool activity is *%s* for this thread. Use `/details on`, `/details off`, or `/details full`.", a.detailsLevel(threadID)))
			return true
		}
		level, ok := parseDetailsLevel(cmd.Args[0])
		if !ok {
			reply("_Usage:_ `/details on|off|full`")
			return true
		}
		a.setDetailsLevel(threadID, level)
		switch level {
		case detailsOff:
			reply("🔇 Hiding the agent's tool activity in this thread.")
		case detailsFull:
			reply("🔎 Showing the agent's tool calls and results in this thread.")
		default:
			reply("🔧 Showing the agent's tool calls in this thread.")
		}
		return true
	}

	return false
}

// handleLoginCommand handles `/login`. It always consumes the command. When
// OBO is disabled it says so rather than dispatching to the agent. An unlinked
// user gets the sign-in prompt; a linked user gets a confirmation of their
// signed-in identity. reply is ephemeral: the identity confirmation carries
// the caller's email, which a shared thread must not see.
func (a *Adapter) handleLoginCommand(ctx context.Context, slackUser, slackChannel, threadID string, reply func(string)) bool {
	if a.OBO == nil {
		reply("_On-behalf-of sign-in is not enabled on this gateway._")
		return true
	}
	if slackUser == "" {
		reply("_Could not determine your Slack user; sign-in is unavailable._")
		return true
	}
	email, linked := a.linkedEmail(ctx, slackUser)
	if !linked {
		// Explicit request: post the sign-in prompt without the nudge throttle.
		a.postSignIn(ctx, slackChannel, threadID, slackUser)
		return true
	}
	if email != "" {
		reply(fmt.Sprintf("✅ _Signed in as *%s*._", escapeMrkdwn(email)))
	} else {
		reply("✅ _Signed in._")
	}
	return true
}

// linkedEmail reports whether slackUser has a muster link and their linked
// email. Sources without the LinkedIdentity extension fall back to a token
// probe (email empty).
func (a *Adapter) linkedEmail(ctx context.Context, slackUser string) (string, bool) {
	if ident, ok := a.OBO.(linkedIdentitySource); ok {
		_, email, linked := ident.LinkedIdentity(slackUser)
		return email, linked
	}
	_, err := a.OBO.TokenFor(ctx, slackUser)
	return "", err == nil
}

// handleLogoutCommand handles `/logout`: it signs the user out of their muster
// link, so the gateway asks them to sign in again before acting as them.
func (a *Adapter) handleLogoutCommand(slackUser string, reply func(string)) bool {
	if a.OBO == nil {
		reply("_On-behalf-of sign-in is not enabled on this gateway._")
		return true
	}
	if slackUser == "" {
		reply("_Could not determine your Slack user; sign-in is unavailable._")
		return true
	}

	a.OBO.Unlink(slackUser)
	reply("👋 Signed out. I'll ask you to `/login` again before I can act as you.")
	return true
}
