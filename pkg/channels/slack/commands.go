package slack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
)

const (
	cmdHelp   = "help"
	cmdStop   = "stop"
	cmdLogin  = "login"  // OBO account linking: sign in
	cmdLogout = "logout" // OBO account linking: sign out
	cmdUsage  = "usage"
	cmdAgent  = "agent" // agent selection; handled by handleAgentSelection, not handleCommand
)

// knownCommands is the verb set the gateway owns.
var knownCommands = map[string]struct{}{
	cmdHelp:   {},
	cmdStop:   {},
	cmdLogin:  {},
	cmdLogout: {},
	cmdUsage:  {},
	cmdAgent:  {},
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
	// Root is set when the command message is its thread's root: a top-level
	// message, whose thread has no replies yet. Slack does not show a
	// thread-scoped ephemeral in such a thread (klaus-gateway#156), so a
	// private reply goes to the channel instead.
	Root bool
}

// parseCommand extracts a leading /command from text. Commands are always
// mention-prefixed in use ("@bot /stop"); StripMention removes the mention
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

// isBareStop reports whether text is the word "stop" on its own — the natural
// reply in a thread the bot answers in without a mention — allowing case and
// trailing punctuation. Only a thread with a running turn reads it as /stop;
// anywhere else it stays what it is today: a message for the agent, or a deny
// word for a paused prompt.
func isBareStop(text string) bool {
	text = strings.TrimRight(strings.TrimSpace(text), ".!?")
	return strings.EqualFold(strings.TrimSpace(text), cmdStop)
}

const helpCommands = "• `/stop` — interrupt the current turn\n" +
	"• `/usage` — show token usage for the last turn and the session\n" +
	"• *Inspect agent steps* (message shortcut: ⋯ menu → Apps, on any message in the thread) — see the tool calls and results behind recent turns, visible only to you\n" +
	"• `/help` — show this message"

// agentHelpText is appended to the help reply when agent selection is available.
const agentHelpText = "\n• `/agent \"<name>\" <question>` — start a new conversation with the named agent; `/agent` alone lists the available agents"

// oboHelpText is appended to the help reply when OBO account linking is enabled.
const oboHelpText = `
• ` + "`/login`" + ` — sign in to Giant Swarm so I act as you
• ` + "`/logout`" + ` — sign out`

// helpText builds the /help reply. botName is the bot's own display name; when
// known the mention example names it ("@Swarmgeist /stop"), otherwise the
// example drops the name rather than hardcoding one.
func helpText(botName string) string {
	var header string
	if botName != "" {
		header = fmt.Sprintf("*Commands* — mention me first, e.g. `@%s /stop`.\n", botName)
	} else {
		header = "*Commands* — mention me first, then the command, e.g. `/stop`.\n"
	}
	return header + helpCommands
}

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
		ephemeralThread := threadID
		if cmd.Root {
			ephemeralThread = ""
		}
		if err := client.postEphemeralText(ctx, slackChannel, slackUser, ephemeralThread, text); err != nil {
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
		access.SetInitiator(ctx, slackChannel, threadID, slackUser)
		if !access.Allowed(ctx, slackChannel, threadID, slackUser) {
			reply(notPermittedNotice)
			return false
		}
		return true
	}

	switch cmd.Name {
	case cmdHelp:
		text := helpText(a.botName(ctx))
		if _, ok := a.AgentCards.(agentCardChecker); ok {
			text += agentHelpText
		}
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
		if a.stopThread(threadID) {
			reply(stopStoppedNotice)
			return true
		}
		// A thread paused on input-required has no in-flight turn to cancel; the
		// paused task must be resolved with a rejection or the tool call dangles.
		// Falling through to dispatch routes "/stop" like a typed "stop" reply,
		// which decisionFromText maps to a structured reject.
		if a.hasPendingTask(threadID) {
			return false
		}
		reply(stopNothingRunningNotice)
		return true

	case cmdUsage:
		if !permittedOnly() {
			return true
		}
		// The model line is read from the kagent controller as the caller: the
		// controller serves nothing to the gateway's own identity.
		reply(a.usageReport(a.withCallerToken(ctx, slackUser), threadID, slackChannel))
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
	// Probe the link for real rather than trusting a store entry: the identity
	// provider may have revoked the token family since (the linker reports that
	// as ErrNotLinked once it has dropped the link), so a dead link re-prompts
	// instead of confirming a sign-in that fails on the next turn. A link store
	// or token endpoint that is briefly away is neither: the person is most
	// likely signed in and is told to retry, not sent through a new sign-in.
	if _, err := a.OBO.TokenFor(ctx, slackUser); err != nil {
		if !errors.Is(err, musterlink.ErrNotLinked) {
			a.Logger.Warn("slack: /login probe failed", "user", slackUser, "error", err)
			reply(tokenErrorNotice)
			return true
		}
		// Explicit request: post the sign-in prompt without the nudge throttle.
		a.postSignIn(ctx, slackChannel, threadID, slackUser, false, signInForLogin)
		return true
	}
	if email := a.linkedEmail(slackUser); email != "" {
		reply(fmt.Sprintf(loginSignedInAsNotice, escapeMrkdwn(email)))
	} else {
		reply(loginSignedInNotice)
	}
	return true
}

// linkedEmail is the linked email of slackUser when the OBO source exposes
// identities, else "".
func (a *Adapter) linkedEmail(slackUser string) string {
	if ident, ok := a.OBO.(linkedIdentitySource); ok {
		_, email, _ := ident.LinkedIdentity(slackUser)
		return email
	}
	return ""
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

	if err := a.OBO.Unlink(slackUser); err != nil {
		a.Logger.Warn("slack: /logout could not remove the link", "user", slackUser, "error", err)
		reply(logoutFailedNotice)
		return true
	}
	reply(logoutNotice)
	return true
}

// withCallerToken seeds ctx with the caller's human token when one can be
// minted, so a read-only lookup at the kagent controller runs as the caller.
// Best-effort: an unlinked caller keeps the plain ctx and the lookup degrades
// the way the reader documents (a cached or omitted value).
func (a *Adapter) withCallerToken(ctx context.Context, slackUser string) context.Context {
	if a.OBO == nil || slackUser == "" {
		return ctx
	}
	token, err := a.OBO.TokenFor(ctx, slackUser)
	if err != nil {
		return ctx
	}
	return pkga2a.WithForwardedToken(ctx, token)
}
