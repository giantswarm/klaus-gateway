package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// agentCardChecker is the optional AgentCardResolver extension that validates
// a selected agent (the agent picker or the slash command's picker): unlike
// CardIdentity it surfaces the card fetch error,
// so an unknown or unreachable agent fails loudly before anything is
// dispatched — never a silent substitute. pkg/a2a.AgentCardClient implements it.
type agentCardChecker interface {
	CardInfo(ctx context.Context, agentRef string) (name, description string, err error)
}

// agent_source values on the turn_dispatch record: how the turn's agent was
// chosen. "replay" is an agent the message already carried when it reached
// dispatch — a picker submission parked for a sign-in and replayed after it —
// "thread" the agent recorded for the thread,
// "default" the configured default agent, "task" the agent replayed from a
// paused task on a button-click resume, and "command"/"shortcut" the agent
// picked in the modal the slash command and the message shortcut open.
const (
	agentSourceReplay   = "replay"
	agentSourceThread   = "thread"
	agentSourceDefault  = "default"
	agentSourceTask     = "task"
	agentSourceCommand  = "command"  // chosen in the slash command's agent picker
	agentSourceShortcut = "shortcut" // chosen in the message shortcut's agent picker
)

// agentUnavailableNotice reports a selection that failed validation. The
// caller appends the current roster when it is available.
const agentUnavailableNotice = "No agent named `%s` is available. Nothing was started; mention the bot with `agents` to list the agents."

// agentNotRunnableNotice reports a selection of an agent that exists but
// cannot start a conversation; %s are the agent's display name and the reason
// the a2a layer gives (a Harness admission or readiness problem).
const agentNotRunnableNotice = "*%s* is installed but cannot start a conversation right now: %s. Nothing was started."

// agentSelectionUnavailable answers the slash command's picker on a
// gateway with no agent-card client to validate names against (A2A not
// configured).
const agentSelectionUnavailable = "Agent selection is not available on this gateway."

// agentValidateTimeout bounds the card fetch that validates a selected agent
// (the picker) before dispatch.
const agentValidateTimeout = 10 * time.Second

// listAgents posts the roster in the thread: the whole answer to the "agents"
// command. Discovery is deliberately ungated, like the help reply: the roster
// is global information, not thread state, so the permittedOnly gate the
// state-changing commands use would only swap this reply for its own in-thread
// refusal — while making the caller the thread initiator as a side effect.
//
// Each row carries a button that starts a conversation with that agent, which
// is what makes the listing the one part of the retired /agent command worth
// keeping: a conversation with a chosen agent starts from here, from the app's
// slash command, or from the message shortcut.
func (a *Adapter) listAgents(ctx context.Context, msg channels.InboundMessage, slackChannel string) {
	err := a.postRoster(ctx, slackChannel, msg.ThreadID, a.conversationStarting(ctx, msg, slackChannel))
	if err == nil {
		return
	}
	notice := agentRosterUnavailable
	if errors.Is(err, pkga2a.ErrNoIdentity) {
		// The controller serves the roster to a human identity; an unlinked
		// caller needs to sign in, and a retry would not help them. A normal
		// state of a caller, not a fault of the roster.
		a.Logger.Info("slack: roster listing needs a signed-in caller", "user", msg.Subject, "thread", msg.ThreadID)
		notice = agentRosterSignIn
	} else {
		a.Logger.Warn("slack: roster listing unavailable", "user", msg.Subject, "thread", msg.ThreadID, "error", err)
	}
	if _, err := a.apiClient().postMessage(ctx, slackChannel, notice, msg.ThreadID); err != nil {
		a.Logger.Warn("slack: post roster listing notice failed", "thread", msg.ThreadID, "error", err)
	}
}

// agentUnavailableLead is the loud selection failure for name.
func agentUnavailableLead(name string) string {
	return fmt.Sprintf(agentUnavailableNotice, strings.ReplaceAll(name, "`", "'"))
}

// agentUnavailableReply is the loud selection failure as text, with the
// current roster when it can be fetched so the user can pick a real name. It
// answers where only text can go (a picker notice through a response_url); a
// thread gets the roster rows (postRoster).
func (a *Adapter) agentUnavailableReply(ctx context.Context, name string) string {
	text := agentUnavailableLead(name)
	if listing, err := a.rosterListing(ctx); err == nil {
		text += "\n\n" + listing
	}
	return text
}

// agentNotRunnableLead is the refusal of an agent that exists but cannot run,
// naming the reason the a2a layer gave. ok is false for any other error, which
// the caller answers with the generic unavailable notice.
func (a *Adapter) agentNotRunnableLead(ctx context.Context, ref string, err error) (string, bool) {
	var ue *pkga2a.AgentUnavailableError
	if !errors.As(err, &ue) {
		return "", false
	}
	// The reason is free text from a Kubernetes condition or a gRPC status: a
	// compile error can span lines and end with a period, and it lands in the
	// middle of one sentence here.
	reason := strings.TrimSuffix(strings.Join(strings.Fields(ue.Reason), " "), ".")
	// The roster's cache can still hold the agent's display name (the picker
	// listed it seconds ago); the technical name the user typed is the fallback.
	return fmt.Sprintf(agentNotRunnableNotice, escapeMrkdwn(a.agentNameFor(ctx, ref)), escapeMrkdwn(reason)), true
}

// agentNotRunnableReply is agentNotRunnableLead as text, with the roster when
// it lists agents so the person can pick one that works; for a picker notice
// through a response_url.
func (a *Adapter) agentNotRunnableReply(ctx context.Context, ref string, err error) (string, bool) {
	text, ok := a.agentNotRunnableLead(ctx, ref, err)
	if !ok {
		return "", false
	}
	if listing, lerr := a.rosterListing(ctx); lerr == nil && listing != agentRosterEmpty {
		text += "\n\n" + listing
	}
	return text, true
}

// defaultAgentNamespace is the namespace of the configured default agent, or
// "" when the default is a bare name (the compose harness style; Start
// refuses that shape when a Roster is configured).
func (a *Adapter) defaultAgentNamespace() string {
	namespace, _, ok := strings.Cut(a.DefaultAgent, "/")
	if !ok {
		return ""
	}
	return namespace
}

// bindThreadAgent records ref as the thread's agent on its row.
func (a *Adapter) bindThreadAgent(ctx context.Context, channelID, threadID, ref string) {
	err := a.gw.UpdateThreadRecord(ctx, ChannelName, channelID, threadID, func(e *store.Entry, _ bool) bool {
		e.AgentRef = ref
		return true
	})
	if err != nil {
		a.Logger.Warn("slack: write agent binding failed", "thread", threadID, "agent", ref, "error", err)
	}
}

// threadAgentBinding is the thread's recorded agent, when its row exists and
// names one.
func (a *Adapter) threadAgentBinding(ctx context.Context, channelID, threadID string) (string, bool) {
	e, ok, err := a.gw.ThreadRecord(ctx, ChannelName, channelID, threadID)
	if err != nil {
		a.Logger.Warn("slack: read thread record failed", "thread", threadID, "error", err)
		return "", false
	}
	return e.AgentRef, ok && e.AgentRef != ""
}

// boundAgentOrDefault is the recorded agent, or the default. For display-only
// callers (the /usage model line).
func (a *Adapter) boundAgentOrDefault(ctx context.Context, channelID, threadID string) string {
	if ref, ok := a.threadAgentBinding(ctx, channelID, threadID); ok {
		return ref
	}
	return a.DefaultAgent
}

// conversationStarting reports whether msg opens a conversation in its thread:
// no record names an agent, because the thread is new or because the gateway
// has forgotten it after its lifetime of silence and it starts over. Only a
// starting message may pick an agent; the roster's Select and the pickers
// refuse a thread that already has one.
func (a *Adapter) conversationStarting(ctx context.Context, msg channels.InboundMessage, slackChannel string) bool {
	_, bound := a.threadAgentBinding(ctx, slackChannel, msg.ThreadID)
	return !bound
}

// threadAgent resolves the agent of a turn that carries no explicit prefix:
// the thread's recorded agent; else the default, which opens the conversation
// (opener) and is recorded for the replies that follow. Inheritance is
// load-bearing: the session's context id embeds the agent ref, so resolving a
// reply to a different agent than its conversation would fork the session. A
// thread the gateway has forgotten has no record, so it starts over on the
// default like any new one.
func (a *Adapter) threadAgent(ctx context.Context, msg channels.InboundMessage, slackChannel string) (ref, source string, opener bool) {
	if bound, ok := a.threadAgentBinding(ctx, slackChannel, msg.ThreadID); ok {
		return bound, agentSourceThread, false
	}
	a.bindThreadAgent(ctx, slackChannel, msg.ThreadID, a.DefaultAgent)
	return a.DefaultAgent, agentSourceDefault, true
}
