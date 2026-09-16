package slack

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// agentCardChecker is the optional AgentCardResolver extension that validates
// a selected agent (the /agent prefix or the slash command's picker): unlike
// CardIdentity it surfaces the card fetch error,
// so an unknown or unreachable agent fails loudly before anything is
// dispatched — never a silent substitute. pkg/a2a.AgentCardClient implements it.
type agentCardChecker interface {
	CardInfo(ctx context.Context, agentRef string) (name, description string, err error)
}

// agent_source values on the turn_dispatch record: how the turn's agent was
// chosen. "prefix" is an /agent prefix on the dispatched message itself,
// "thread" the agent recorded for the thread,
// "default" the configured default agent, and "task" the agent replayed from a
// paused task on a button-click resume.
const (
	agentSourcePrefix  = "prefix"
	agentSourceThread  = "thread"
	agentSourceDefault = "default"
	agentSourceTask    = "task"
	agentSourceCommand = "command" // chosen in the slash command's agent picker
)

// agentSwitchRefusal answers an /agent prefix inside an existing conversation
// that names a DIFFERENT agent (or one that cannot be resolved). The session's
// context id embeds the agent ref, so a mid-conversation switch would silently
// start an empty session; refusing is kinder than that. Re-selecting the
// conversation's own agent is no switch at all and dispatches normally (see
// handleAgentReselection).
const agentSwitchRefusal = "_This conversation already has its agent, and switching mid-conversation would lose its context. Start a new conversation — a fresh mention, or a new chat — with_ `/agent \"<name>\" <question>`."

// agentNothingSelectedHint answers a name-only "/agent <name>": with no
// question there is nothing to dispatch, so no conversation starts and no
// binding exists for later messages to inherit. It must say so explicitly — a
// user who typed the name and their question as two messages will otherwise
// reasonably assume the selection stuck.
const agentNothingSelectedHint = "Nothing was selected — include your question in the same message: `/agent %s <question>`."

// agentUnavailableNotice reports a selection that failed validation. The
// caller appends the current roster when it is available.
const agentUnavailableNotice = "⚠️ I don't know an agent named `%s` (or it isn't reachable right now), so I haven't started anything."

// agentSelectionUnavailable answers /agent and the slash command's picker on a
// gateway with no agent-card client to validate names against (A2A not
// configured).
const agentSelectionUnavailable = "_Agent selection isn't available on this gateway._"

// agentResolveCheckFailedNotice is posted when a quoted selection could not be
// resolved because the roster fetch failed. Nothing is dispatched: guessing an
// agent would violate loud-never-substituted.
const agentResolveCheckFailedNotice = "⚠️ _I couldn't check the available agents just now, so I haven't started anything. Please try again._"

// agentAmbiguousNotice reports a quoted selection matching more than one
// agent. The technical names disambiguate, so they are listed here even though
// the roster itself shows display names only.
const agentAmbiguousNotice = "⚠️ *%s* matches more than one agent, so I haven't started anything. Pick one by its technical name:"

// agentValidateTimeout bounds the card fetch that validates a selected agent
// (/agent prefix or picker) before dispatch.
const agentValidateTimeout = 10 * time.Second

// handleAgentSelection processes the /agent command. Unlike the consumed
// commands (login, details, …) the select form mutates msg — stamps the chosen
// agent ref and strips the prefix from the text — and reports dispatch=true so
// the remainder of the message is dispatched as the conversation's first turn.
// Every other form (bare roster listing, name-only hint, in-conversation
// refusal, failed validation) replies in-thread and consumes the message.
func (a *Adapter) handleAgentSelection(ctx context.Context, cmd *slashCommand, msg *channels.InboundMessage, slackChannel string) (dispatch bool) {
	reply := func(text string) {
		if _, err := a.apiClient().postMessage(ctx, slackChannel, text, msg.ThreadID); err != nil {
			a.Logger.Warn("slack: post agent-selection reply failed", "thread", msg.ThreadID, "error", err)
		}
	}

	// Bare "/agent": list the roster. Discovery is deliberately ungated, like
	// /help: the roster is global information, not thread state, so the
	// permittedOnly gate the state-changing commands use would only swap this
	// reply for its own in-thread refusal — while making the caller the thread
	// initiator as a side effect. The refusal and hint branches below are
	// equally ungated for the same reason: none of them changes any state.
	if len(cmd.Args) == 0 {
		if a.Roster == nil {
			reply(agentSelectionUnavailable)
			return false
		}
		listing, ok := a.rosterListing(ctx)
		if !ok {
			reply(agentRosterUnavailable)
			return false
		}
		reply(listing)
		return false
	}

	// A conversation is bound to its agent for life; selection only rides the
	// message that opens one — in any thread, root or reply.
	starting := a.conversationStarting(ctx, *msg, slackChannel)
	if !starting {
		return a.handleAgentReselection(ctx, reply, msg, slackChannel)
	}

	name, quoted, question := splitAgentCommand(msg.Text)
	if question == "" {
		hintName := "<name>"
		switch {
		case quoted && name != "":
			hintName = `"` + name + `"`
		case !quoted:
			if _, ok := a.agentRefFromName(name); ok {
				hintName = strings.ToLower(name)
			}
		}
		reply(fmt.Sprintf(agentNothingSelectedHint, hintName))
		return false
	}

	ref, resolved := a.resolveSelection(ctx, reply, name, quoted, msg.ThreadID)
	if !resolved {
		return false
	}

	checker, ok := a.AgentCards.(agentCardChecker)
	if !ok {
		reply(agentSelectionUnavailable)
		return false
	}
	vctx, cancel := context.WithTimeout(ctx, agentValidateTimeout)
	defer cancel()
	if _, _, err := checker.CardInfo(vctx, ref); err != nil {
		a.Logger.Info("slack: agent selection failed validation", "agent", ref, "thread", msg.ThreadID, "error", err)
		reply(a.agentUnavailableReply(ctx, name))
		return false
	}

	a.bindThreadAgent(ctx, slackChannel, msg.ThreadID, ref)
	msg.AgentRef = ref
	msg.Opener = true
	msg.Text = question
	return true
}

// handleAgentReselection handles an /agent prefix inside an existing
// conversation. Naming the conversation's OWN agent is a no-op re-selection,
// not a switch — users repeat the prefix out of muscle memory or copy-paste —
// so the turn dispatches like an unprefixed reply: same agent, same session,
// nothing re-announced. Everything else keeps the switch refusal: a different
// agent (the session's context id embeds the agent ref, so an actual switch
// would silently start an empty session), a name-only form, and a selector
// that does not resolve (unknown, ambiguous, or roster unreachable — without a
// confirmed match to the current agent, dispatching would risk exactly that
// switch). Resolution failures stay quiet here — no roster listing lands
// mid-thread, because nothing mid-conversation can select anyway.
func (a *Adapter) handleAgentReselection(ctx context.Context, reply func(string), msg *channels.InboundMessage, slackChannel string) (dispatch bool) {
	name, quoted, question := splitAgentCommand(msg.Text)
	if question == "" {
		reply(agentSwitchRefusal)
		return false
	}
	ref, ok := a.resolveSelection(ctx, func(string) {}, name, quoted, msg.ThreadID)
	if !ok {
		reply(agentSwitchRefusal)
		return false
	}
	current, _, _ := a.threadAgent(ctx, *msg, slackChannel)
	if current != ref {
		reply(agentSwitchRefusal)
		return false
	}
	msg.AgentRef = ref
	msg.Text = question
	return true
}

// resolveSelection maps a parsed /agent selector to the ref to dispatch to. A
// quoted name selects by display name (or technical name), resolved against
// the live roster; an unquoted name is the technical form, built syntactically
// (the caller validates it against the agent card). ok is false when the
// selection failed — the failure has already been replied in-thread, loudly:
// no match, an ambiguous name, and an unreachable roster all consume the
// message rather than substitute an agent.
func (a *Adapter) resolveSelection(ctx context.Context, reply func(string), name string, quoted bool, threadID string) (ref string, ok bool) {
	if !quoted {
		ref, validName := a.agentRefFromName(name)
		if !validName {
			reply(a.agentUnavailableReply(ctx, name))
			return "", false
		}
		return ref, true
	}
	if a.Roster == nil {
		reply(agentSelectionUnavailable)
		return "", false
	}
	refs, err := a.agentRefsForSelector(ctx, name)
	if err != nil {
		a.Logger.Warn("slack: agent selector resolution failed", "selector", name, "thread", threadID, "error", err)
		reply(agentResolveCheckFailedNotice)
		return "", false
	}
	switch len(refs) {
	case 0:
		reply(a.agentUnavailableReply(ctx, name))
		return "", false
	case 1:
		return refs[0], true
	default:
		reply(agentAmbiguousReply(name, refs))
		return "", false
	}
}

// agentUnavailableReply renders the loud selection failure, including the
// current roster when it can be fetched so the user can pick a real name.
func (a *Adapter) agentUnavailableReply(ctx context.Context, name string) string {
	text := fmt.Sprintf(agentUnavailableNotice, strings.ReplaceAll(name, "`", "'"))
	if listing, ok := a.rosterListing(ctx); ok {
		text += "\n\n" + listing
	}
	return text
}

// agentAmbiguousReply renders the loud ambiguous-selection failure with the
// technical selectors that disambiguate.
func agentAmbiguousReply(name string, refs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, agentAmbiguousNotice, escapeMrkdwn(name))
	for _, ref := range refs {
		b.WriteString("\n• `/agent " + ref + " <question>`")
	}
	return b.String()
}

// agentNamePartRe matches one DNS-1123 label, the shape of kagent agent names
// and namespaces. Anything else is rejected before it can reach a URL path.
var agentNamePartRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// agentRefFromName resolves a user-typed agent name to the agentRef the A2A
// clients use. Matching is on the technical (DNS-1123) name, case-insensitive.
// The result follows the deployment's ref shape (refShape): a bare name, or
// one naming the served namespace, renders in that shape; any other
// "namespace/name" is used as typed. ok is false for anything that is not a
// well-formed name.
func (a *Adapter) agentRefFromName(raw string) (ref string, ok bool) {
	name := strings.ToLower(strings.TrimSpace(raw))
	namespace := ""
	if ns, rest, found := strings.Cut(name, "/"); found {
		namespace, name = ns, rest
		if !agentNamePartRe.MatchString(namespace) {
			return "", false
		}
	}
	if !agentNamePartRe.MatchString(name) {
		return "", false
	}
	// A typed namespace that is the served one is the same as none; a foreign
	// one stays as typed so the controller refuses it.
	if namespace == a.Namespace {
		namespace = ""
	}
	return a.refShape(namespace, name), true
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

// agentQuotePairs maps an opening quote to its closing partner. Curly quotes
// are included because Slack clients autoformat straight quotes as the user
// types.
var agentQuotePairs = map[rune]rune{'"': '"', '“': '”', '\'': '\'', '‘': '’'}

// splitAgentCommand splits "/agent <name> <question…>" into the name, whether
// it was quoted, and the question with its original formatting preserved
// (parseCommand's Fields split would collapse the question's newlines). A
// quoted name — straight or Slack smart quotes — may contain spaces and
// selects by display name; an unquoted name is one whitespace token, the
// technical (DNS-1123) form. An unterminated quote falls back to the token
// split, which fails resolution loudly instead of guessing where the name
// ends. The caller has already matched the verb via parseCommand.
func splitAgentCommand(text string) (name string, quoted bool, question string) {
	rest := strings.TrimSpace(text)
	// parseCommand tolerates whitespace between the slash and the verb
	// ("/ agent …" still parses), so trim it here too before slicing the verb
	// off, or the slice lands mid-word.
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "/"))
	rest = strings.TrimSpace(rest[len(cmdAgent):])
	if rest == "" {
		return "", false, ""
	}
	if open, width := utf8.DecodeRuneInString(rest); width > 0 {
		if closing, ok := agentQuotePairs[open]; ok {
			body := rest[width:]
			if i := strings.IndexRune(body, closing); i >= 0 {
				return strings.TrimSpace(body[:i]), true, strings.TrimSpace(body[i+utf8.RuneLen(closing):])
			}
		}
	}
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		return rest[:i], false, strings.TrimSpace(rest[i:])
	}
	return rest, false, ""
}

// bindThreadAgent records ref as the thread's agent in its thread record.
func (a *Adapter) bindThreadAgent(ctx context.Context, channelID, threadID, ref string) {
	mu := a.recordLock(channelID, threadID)
	mu.Lock()
	defer mu.Unlock()
	rec := a.records()
	r, _, err := rec.ThreadRecord(ctx, ChannelName, channelID, threadID)
	if err != nil {
		a.Logger.Warn("slack: read thread record for binding failed", "thread", threadID, "error", err)
	}
	r.AgentRef = ref
	if err := rec.SaveThreadRecord(ctx, ChannelName, channelID, threadID, r.Thread); err != nil {
		a.Logger.Warn("slack: write agent binding failed", "thread", threadID, "agent", ref, "error", err)
	}
}

// threadAgentBinding is the thread's recorded agent, when its record exists
// and names one.
func (a *Adapter) threadAgentBinding(ctx context.Context, channelID, threadID string) (string, bool) {
	r, ok, err := a.records().ThreadRecord(ctx, ChannelName, channelID, threadID)
	if err != nil {
		a.Logger.Warn("slack: read thread record failed", "thread", threadID, "error", err)
		return "", false
	}
	return r.AgentRef, ok && r.AgentRef != ""
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
// starting message may carry /agent; a re-selection of the thread's own agent
// is a no-op, anything else a refused switch.
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
