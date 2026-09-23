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

// helpCommand is one command in the help reply: the command as it is typed,
// and what it does.
type helpCommand struct{ command, effect string }

// helpGroup is one group of the help reply, named by what the person is doing.
type helpGroup struct {
	title    string
	commands []helpCommand
}

// helpGroups lists the commands the gateway serves, grouped by what the
// person is doing: agent selection and sign-in only when this gateway has
// them.
func helpGroups(agents, signIn bool) []helpGroup {
	groups := []helpGroup{{title: "In a thread", commands: []helpCommand{
		{"/stop", "Interrupt the running turn"},
		{"/usage", "Tokens for the last turn and the session"},
	}}}
	if agents {
		groups = append(groups, helpGroup{title: "Agents", commands: []helpCommand{
			{"/agent", "List the agents"},
			{`/agent "Name" question`, "Start a conversation with a named agent"},
		}})
	}
	if signIn {
		groups = append(groups, helpGroup{title: "Account", commands: []helpCommand{
			{"/login", "Sign in to Giant Swarm; the agent then acts with your permissions"},
			{"/logout", "Sign out"},
		}})
	}
	return groups
}

// helpShortcutNote names the one feature that is a shortcut, not a command.
const helpShortcutNote = "Inspect agent steps: open the ⋯ menu on any message in the thread, then Apps. Visible only to you."

// helpBlocks builds the /help reply: a header, how to address the bot, one
// group per activity with the command as a code label and its effect as the
// text, and the Inspect shortcut. botName is the bot's own display name; when
// known the mention names it, otherwise it says "the bot" rather than
// hardcoding one. The returned text is the notification fallback.
func helpBlocks(botName string, agents, signIn bool) (string, []any) {
	mention := "the bot"
	if botName != "" {
		mention = "@" + botName
	}
	var lines []string
	var elements []any
	for _, g := range helpGroups(agents, signIn) {
		elements = append(elements, map[string]any{
			bkType:     "rich_text_section",
			bkElements: []any{map[string]any{bkType: bkText, bkText: g.title, bkStyle: map[string]any{"bold": true}}},
		})
		items := make([]any, 0, len(g.commands))
		for _, c := range g.commands {
			lines = append(lines, c.command)
			items = append(items, map[string]any{
				bkType: "rich_text_section",
				bkElements: []any{
					map[string]any{bkType: bkText, bkText: c.command, bkStyle: map[string]any{"code": true}},
					map[string]any{bkType: bkText, bkText: "  " + c.effect},
				},
			})
		}
		elements = append(elements, map[string]any{bkType: "rich_text_list", bkStyle: "bullet", bkElements: items})
	}
	blocks := []any{
		map[string]any{bkType: "header", bkText: plainTextObj("Commands")},
		contextBlock(fmt.Sprintf("In a channel, mention %s first. In a direct message, type the command.", mention)),
		map[string]any{bkType: "rich_text", bkElements: elements},
		contextBlock(helpShortcutNote),
	}
	return "Commands: " + strings.Join(lines, ", "), blocks
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
	// note answers a command whose reply is one short gateway line (a stop, a
	// refusal) in the metadata register.
	note := func(text string) {
		if _, err := client.postNote(ctx, slackChannel, text, threadID); err != nil {
			a.Logger.Warn("slack: post command note failed", "error", err)
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
			note(notPermittedNotice)
			return false
		}
		return true
	}

	switch cmd.Name {
	case cmdHelp:
		_, agents := a.AgentCards.(agentCardChecker)
		text, blocks := helpBlocks(a.botName(ctx), agents, a.OBO != nil)
		if _, err := client.postBlocks(ctx, slackChannel, threadID, text, blocks); err != nil {
			a.Logger.Warn("slack: post help failed", "error", err)
		}
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
			note(stopStoppedNotice)
			return true
		}
		// A thread paused on input-required has no in-flight turn to cancel; the
		// paused task must be resolved with a rejection or the tool call dangles.
		// Falling through to dispatch routes "/stop" like a typed "stop" reply,
		// which decisionFromText maps to a structured reject.
		if a.hasPendingTask(threadID) {
			return false
		}
		note(stopNothingRunningNotice)
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
		reply(oboDisabledNotice)
		return true
	}
	if slackUser == "" {
		reply(noSlackUserNotice)
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
		reply(oboDisabledNotice)
		return true
	}
	if slackUser == "" {
		reply(noSlackUserNotice)
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
