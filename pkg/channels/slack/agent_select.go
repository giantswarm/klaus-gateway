package slack

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// AgentRosterSource lists the agents selectable from Slack, discovered
// dynamically from the kagent controller (never a static gateway config list).
// Implemented by pkg/a2a.AgentsClient; nil disables the roster listing.
type AgentRosterSource interface {
	ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error)
}

// agentCardChecker is the optional AgentCardResolver extension that validates
// an /agent selection: unlike CardIdentity it surfaces the card fetch error,
// so an unknown or unreachable agent fails loudly before anything is
// dispatched — never a silent substitute. pkg/a2a.AgentCardClient implements it.
type agentCardChecker interface {
	CardInfo(ctx context.Context, agentRef string) (name, description string, err error)
}

// agent_source values on the turn_dispatch record: how the turn's agent was
// chosen. "prefix" is an /agent prefix on the dispatched message itself,
// "thread" the binding inherited from the conversation's root message,
// "default" the configured default agent, and "task" the agent replayed from a
// paused task on a button-click resume.
const (
	agentSourcePrefix  = "prefix"
	agentSourceThread  = "thread"
	agentSourceDefault = "default"
	agentSourceTask    = "task"
)

// agentSwitchRefusal answers an /agent prefix inside an existing conversation.
// The session's context id embeds the agent ref, so a mid-conversation switch
// would silently start an empty session; refusing is kinder than that.
const agentSwitchRefusal = "_This conversation already has its agent, and switching mid-conversation would lose its context. Start a new conversation — a fresh mention, or a new chat — with_ `/agent <name> <question>`."

// agentNothingSelectedHint answers a name-only "/agent <name>": with no
// question there is nothing to dispatch, so no conversation starts and no
// binding exists for later messages to inherit. It must say so explicitly — a
// user who typed the name and their question as two messages will otherwise
// reasonably assume the selection stuck.
const agentNothingSelectedHint = "Nothing was selected — include your question in the same message: `/agent %s <question>`."

// agentUnavailableNotice reports a selection that failed validation. The
// caller appends the current roster when it is available.
const agentUnavailableNotice = "⚠️ I don't know an agent named `%s` (or it isn't reachable right now), so I haven't started anything."

// agentSelectionUnavailable answers /agent on a gateway with no agent-card
// client to validate names against (A2A not configured).
const agentSelectionUnavailable = "_Agent selection isn't available on this gateway._"

// agentRosterUnavailable answers a bare /agent when the roster fetch failed.
const agentRosterUnavailable = "_I can't list the available agents right now. Please try again in a moment._"

// agentRosterEmpty answers a bare /agent when the controller reports no agents.
const agentRosterEmpty = "_No agents are installed right now._"

// agentValidateTimeout bounds the card fetch that validates an /agent
// selection before dispatch.
const agentValidateTimeout = 10 * time.Second

// rosterListTimeout bounds a roster rendering pass: the list-agents call plus
// the per-agent card lookups joined into it.
const rosterListTimeout = 5 * time.Second

// rosterTTL is how long a fetched roster is served from cache. Short, so a
// newly installed agent becomes selectable without a redeploy while a burst of
// /agent listings costs one controller call.
const rosterTTL = 30 * time.Second

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

	// Bare "/agent": list the roster. Discovery works anywhere, like /help.
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
	// conversation-starting message (a channel mention that starts a reply
	// thread, or the first message of a new assistant-pane chat).
	if msg.ThreadID != msg.MessageID {
		reply(agentSwitchRefusal)
		return false
	}

	name, question := splitAgentCommand(msg.Text)
	ref, validName := a.agentRefFromName(name)
	if question == "" {
		hintName := "<name>"
		if validName {
			hintName = strings.ToLower(name)
		}
		reply(fmt.Sprintf(agentNothingSelectedHint, hintName))
		return false
	}
	if !validName {
		reply(a.agentUnavailableReply(ctx, name))
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

	a.bindThreadAgent(msg.ThreadID, ref)
	msg.AgentRef = ref
	msg.Text = question
	return true
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

// rosterListing renders the selectable-agent roster: display names from the
// AgentCards, technical names, and the Agent CRs' descriptions. ok is false
// when no roster source is configured or the fetch failed.
func (a *Adapter) rosterListing(ctx context.Context) (string, bool) {
	agents, err := a.rosterAgents(ctx)
	if err != nil {
		return "", false
	}
	if len(agents) == 0 {
		return agentRosterEmpty, true
	}

	// One deadline for the whole pass: the per-agent card lookups are cached,
	// but a cold cache against an unreachable card endpoint must not stack up
	// full HTTP timeouts.
	ctx, cancel := context.WithTimeout(ctx, rosterListTimeout)
	defer cancel()

	var b strings.Builder
	b.WriteString("*Available agents* — start a new conversation with `/agent <name> <question>`:")
	for _, ag := range agents {
		selector := ag.Name
		ref := ag.Name
		if ag.Namespace != "" {
			ref = ag.Namespace + "/" + ag.Name
			// A bare name resolves in the default agent's namespace; agents
			// elsewhere are selected by their full ref.
			if ag.Namespace != a.defaultAgentNamespace() {
				selector = ref
			}
		}
		display := ""
		if a.AgentCards != nil {
			display, _ = a.AgentCards.CardIdentity(ctx, ref)
		}
		if display != "" && !strings.EqualFold(display, ag.Name) {
			b.WriteString("\n• *" + escapeMrkdwn(display) + "* (`" + selector + "`)")
		} else {
			b.WriteString("\n• `" + selector + "`")
		}
		if ag.Description != "" {
			b.WriteString(" — " + escapeMrkdwn(ag.Description))
		}
	}
	return b.String(), true
}

// rosterAgents returns the roster, served from a brief cache so a burst of
// listings costs one controller call while a newly installed agent still
// appears without a redeploy.
func (a *Adapter) rosterAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	if a.Roster == nil {
		return nil, fmt.Errorf("slack: no agent roster source configured")
	}
	now := time.Now()
	a.rosterMu.Lock()
	if a.rosterCached != nil && now.Before(a.rosterExpires) {
		cached := a.rosterCached
		a.rosterMu.Unlock()
		return cached, nil
	}
	a.rosterMu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, rosterListTimeout)
	defer cancel()
	agents, err := a.Roster.ListAgents(lctx)
	if err != nil {
		return nil, err
	}
	a.rosterMu.Lock()
	a.rosterCached, a.rosterExpires = agents, time.Now().Add(rosterTTL)
	a.rosterMu.Unlock()
	return agents, nil
}

// agentNamePartRe matches one DNS-1123 label, the shape of kagent agent names
// and namespaces. Anything else is rejected before it can reach a URL path.
var agentNamePartRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// agentRefFromName resolves a user-typed agent name to the agentRef the A2A
// clients use. Matching is on the technical (DNS-1123) name, case-insensitive.
// A bare name is completed with the default agent's namespace; an explicit
// "namespace/name" is used as typed. ok is false for anything that is not a
// well-formed name.
func (a *Adapter) agentRefFromName(raw string) (ref string, ok bool) {
	name := strings.ToLower(strings.TrimSpace(raw))
	namespace := a.defaultAgentNamespace()
	if ns, rest, found := strings.Cut(name, "/"); found {
		namespace, name = ns, rest
		if !agentNamePartRe.MatchString(namespace) {
			return "", false
		}
	}
	if !agentNamePartRe.MatchString(name) {
		return "", false
	}
	if namespace == "" {
		return name, true
	}
	return namespace + "/" + name, true
}

// defaultAgentNamespace is the namespace of the configured default agent, or
// "" when the default is a bare name (the compose harness style).
func (a *Adapter) defaultAgentNamespace() string {
	namespace, _, ok := strings.Cut(a.DefaultAgent, "/")
	if !ok {
		return ""
	}
	return namespace
}

// splitAgentCommand splits "/agent <name> <question…>" into the name token and
// the question with its original formatting preserved (parseCommand's Fields
// split would collapse the question's newlines). The caller has already
// matched the verb via parseCommand.
func splitAgentCommand(text string) (name, question string) {
	rest := strings.TrimSpace(text)
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.TrimSpace(rest[len(cmdAgent):])
	if rest == "" {
		return "", ""
	}
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		return rest[:i], strings.TrimSpace(rest[i:])
	}
	return rest, ""
}

// bindThreadAgent records threadID's conversation→agent binding. An empty ref
// records a checked root with no prefix (default agent), so replies skip the
// root fetch. Entries idle past threadStateTTL are evicted on insert.
func (a *Adapter) bindThreadAgent(threadID, ref string) {
	now := time.Now()
	a.bindingMu.Lock()
	defer a.bindingMu.Unlock()
	if a.agentBindings == nil {
		a.agentBindings = make(map[string]ttlEntry[string])
	}
	sweepExpired(a.agentBindings, now)
	a.agentBindings[threadID] = ttlEntry[string]{value: ref, expires: now.Add(threadStateTTL)}
}

// threadAgentBinding returns threadID's recorded binding. Reading refreshes
// the deadline, like detailsLevel: a conversation in active use never loses
// its agent mid-conversation, only idle ones are evicted.
func (a *Adapter) threadAgentBinding(threadID string) (ref string, ok bool) {
	a.bindingMu.Lock()
	defer a.bindingMu.Unlock()
	entry, ok := a.agentBindings[threadID]
	if !ok {
		return "", false
	}
	entry.expires = time.Now().Add(threadStateTTL)
	a.agentBindings[threadID] = entry
	return entry.value, true
}

// rootAgentLookupTimeout bounds the conversations.replies call that recovers a
// conversation's agent binding from its root message after a restart, so a
// slow Slack API cannot stall inbound handling.
const rootAgentLookupTimeout = 3 * time.Second

// threadAgent resolves the agent for a turn that carries no explicit /agent
// prefix: the conversation's recorded binding, the binding re-derived from the
// conversation's root message (where any prefix is visible — the recovery path
// after a restart or TTL sweep), or the configured default. Follow-up
// inheritance is load-bearing: the session's context id embeds the agent ref,
// so resolving a reply to a different agent than its conversation would fork
// the session. When the root cannot be fetched the turn degrades to the
// default agent — a Slack API flake must not block whole threads, and almost
// all conversations are default-bound — but the fallback is NOT cached, so the
// next reply re-derives the real binding instead of the conversation staying
// mis-bound.
func (a *Adapter) threadAgent(ctx context.Context, msg channels.InboundMessage, slackChannel string) (ref, source string) {
	if bound, found := a.threadAgentBinding(msg.ThreadID); found {
		if bound == "" {
			return a.DefaultAgent, agentSourceDefault
		}
		return bound, agentSourceThread
	}
	// A conversation-starting message with no prefix: the default, recorded so
	// replies skip the root fetch.
	if msg.ThreadID == msg.MessageID {
		a.bindThreadAgent(msg.ThreadID, "")
		return a.DefaultAgent, agentSourceDefault
	}
	rctx, cancel := context.WithTimeout(ctx, rootAgentLookupTimeout)
	defer cancel()
	rootText, err := a.apiClient().threadRootText(rctx, slackChannel, msg.ThreadID)
	if err != nil {
		a.Logger.Warn("slack: conversation root lookup for agent binding failed, using default agent uncached",
			"thread", msg.ThreadID, "error", err)
		return a.DefaultAgent, agentSourceDefault
	}
	bound := a.rootAgentRef(rootText)
	a.bindThreadAgent(msg.ThreadID, bound)
	if bound == "" {
		return a.DefaultAgent, agentSourceDefault
	}
	return bound, agentSourceThread
}

// rootAgentRef extracts the agent binding from a conversation root's text: the
// resolved ref of a complete "/agent <name> <question>" prefix, or "" (default
// agent) for anything else. A name-only or malformed prefix never started a
// conversation, so it binds nothing.
func (a *Adapter) rootAgentRef(rootText string) string {
	cmd := parseCommand(StripMention(rootText))
	if cmd == nil || cmd.Name != cmdAgent || len(cmd.Args) < 2 {
		return ""
	}
	ref, ok := a.agentRefFromName(cmd.Args[0])
	if !ok {
		return ""
	}
	return ref
}
