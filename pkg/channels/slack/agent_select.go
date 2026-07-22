package slack

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

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

// agentCheckFailedNotice is posted when a DM selection could not be verified
// as conversation-starting (the thread scan failed). Nothing is dispatched:
// accepting blindly could rebind an existing conversation and fork its session.
const agentCheckFailedNotice = "⚠️ _I couldn't check this conversation just now, so I haven't started anything. Please try again._"

// agentValidateTimeout bounds the card fetch that validates an /agent
// selection before dispatch.
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
	// conversation-starting message (a channel mention that starts a reply
	// thread, or the first message of a new assistant-pane chat).
	starting, err := a.conversationStarting(ctx, *msg, slackChannel)
	if err != nil {
		reply(agentCheckFailedNotice)
		return false
	}
	if !starting {
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
		reply(a.agentUnavailableReply(ctx, name, question))
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
		reply(a.agentUnavailableReply(ctx, name, question))
		return false
	}

	a.bindThreadAgent(msg.ThreadID, ref)
	msg.AgentRef = ref
	msg.Text = question
	return true
}

// agentUnavailableReply renders the loud selection failure: a did-you-mean
// suggestion when the typed name looks like an agent's display name, and the
// current roster when it can be fetched so the user can pick a real name.
func (a *Adapter) agentUnavailableReply(ctx context.Context, name, question string) string {
	text := fmt.Sprintf(agentUnavailableNotice, strings.ReplaceAll(name, "`", "'"))
	if suggestion, ok := a.didYouMeanSuggestion(ctx, name, question); ok {
		text += "\n" + suggestion
	}
	if listing, ok := a.rosterListing(ctx); ok {
		text += "\n\n" + listing
	}
	return text
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
// "" when the default is a bare name (the compose harness style; Start
// refuses that shape when a Roster is configured).
func (a *Adapter) defaultAgentNamespace() string {
	namespace, _, ok := strings.Cut(a.DefaultAgent, "/")
	if !ok {
		return ""
	}
	return namespace
}

// splitAgentCommand splits "/agent <name> <question…>" into the name token and
// the question with its original formatting preserved (parseCommand's Fields
// split would collapse the question's newlines). The name is one whitespace
// token, full stop: selection is by technical (DNS-1123) name, so there is no
// quoting grammar — a quoted or multi-word name fails validation and lands on
// the did-you-mean suggestion, whose normalization strips quotes anyway. The
// caller has already matched the verb via parseCommand.
func splitAgentCommand(text string) (name, question string) {
	rest := strings.TrimSpace(text)
	// parseCommand tolerates whitespace between the slash and the verb
	// ("/ agent …" still parses), so trim it here too before slicing the verb
	// off, or the slice lands mid-word.
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "/"))
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
// root fetch. Entries idle past threadStateTTL are evicted on insert. The map
// is deliberately in-memory only: the binding is derivable state (the /agent
// prefix is visible in the conversation's opening message on Slack itself),
// and threadAgent re-derives it after a restart or eviction.
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

// boundAgentOrDefault is the agent a thread's turns resolve to, without the
// root-fetch recovery: the recorded binding, or the default. For display-only
// callers (the /usage model line, the sign-in hand-off notice) where a
// post-restart cache miss naming the default for a bound thread is cosmetic,
// not routing.
func (a *Adapter) boundAgentOrDefault(threadID string) string {
	if bound, ok := a.threadAgentBinding(threadID); ok && bound != "" {
		return bound
	}
	return a.DefaultAgent
}

// rootAgentLookupTimeout bounds the conversations.replies call that recovers a
// conversation's agent binding from its root message after a restart, so a
// slow Slack API cannot stall inbound handling.
const rootAgentLookupTimeout = 3 * time.Second

// conversationStarting reports whether msg starts a new conversation — the
// only place an /agent prefix binds. In a channel that is a mention rooting
// its own reply thread. In the assistant pane every message carries a
// Slack-managed thread anchor as thread_ts, allocated when the chat opens
// (klaus-gateway#157) — the chat's first message is never its own root — so
// root equality is meaningless there: a DM message starts the conversation
// when no conversation state exists in-process and no earlier human message
// precedes it in the thread (the post-restart check, one bounded API call on
// the rare prefixed-DM path).
func (a *Adapter) conversationStarting(ctx context.Context, msg channels.InboundMessage, slackChannel string) (bool, error) {
	if msg.ThreadID == msg.MessageID {
		return true, nil
	}
	if !isDMChannelID(slackChannel) {
		return false, nil
	}
	if _, found := a.threadAgentBinding(msg.ThreadID); found {
		return false, nil
	}
	if a.isActiveThread(msg.ThreadID) {
		return false, nil
	}
	rctx, cancel := context.WithTimeout(ctx, rootAgentLookupTimeout)
	defer cancel()
	firstTS, _, err := a.apiClient().threadFirstHumanMessage(rctx, slackChannel, msg.ThreadID)
	if err != nil {
		a.Logger.Warn("slack: conversation-start check failed", "thread", msg.ThreadID, "error", err)
		return false, err
	}
	// No earlier human message (an empty ts means the scan found none — a
	// fresh chat whose own message hasn't landed in the replies view yet), or
	// this message IS the first: the conversation starts here.
	return firstTS == "" || firstTS == msg.MessageID, nil
}

// threadAgent resolves the agent for a turn that carries no explicit /agent
// prefix: the conversation's recorded binding, the binding re-derived from the
// conversation's opening message (where any prefix is visible — the recovery
// path after a restart or TTL sweep), or the configured default. The opening
// message is the thread root in a channel, but the first HUMAN message in a
// DM: the assistant pane roots threads at a Slack-managed anchor, not the
// user's first message. Follow-up inheritance is load-bearing: the session's
// context id embeds the agent ref, so resolving a reply to a different agent
// than its conversation would fork the session. When the opening message
// cannot be fetched the turn degrades to the default agent — a Slack API
// flake must not block whole threads, and almost all conversations are
// default-bound — but the fallback is NOT cached, so the next reply
// re-derives the real binding instead of the conversation staying mis-bound.
// opener reports whether msg itself opened its conversation: true for a
// channel root mention, and for a DM whose thread has no earlier human
// message (the assistant pane's first message is never its own thread root,
// so root equality cannot tell). Dispatch uses it to skip reply-only work —
// the resume existence-check must not greet every new pane chat with
// "starting fresh".
func (a *Adapter) threadAgent(ctx context.Context, msg channels.InboundMessage, slackChannel string) (ref, source string, opener bool) {
	if bound, found := a.threadAgentBinding(msg.ThreadID); found {
		if bound == "" {
			return a.DefaultAgent, agentSourceDefault, false
		}
		return bound, agentSourceThread, false
	}
	// A conversation-starting message with no prefix: the default, recorded so
	// replies skip the opening-message fetch.
	if msg.ThreadID == msg.MessageID {
		a.bindThreadAgent(msg.ThreadID, "")
		return a.DefaultAgent, agentSourceDefault, true
	}
	rctx, cancel := context.WithTimeout(ctx, rootAgentLookupTimeout)
	defer cancel()
	var openingTS, openingText string
	var err error
	if isDMChannelID(slackChannel) {
		openingTS, openingText, err = a.apiClient().threadFirstHumanMessage(rctx, slackChannel, msg.ThreadID)
	} else {
		// Channels keep strict root derivation: a refused /agent reply still
		// exists as thread text, and a human-message scan would resurrect it.
		openingText, err = a.apiClient().threadRootText(rctx, slackChannel, msg.ThreadID)
	}
	if err != nil {
		a.Logger.Warn("slack: conversation opening-message lookup for agent binding failed, using default agent uncached",
			"thread", msg.ThreadID, "error", err)
		return a.DefaultAgent, agentSourceDefault, false
	}
	// In a DM the scanned opener may be this very message (a new pane chat);
	// an empty ts means the scan saw no human message at all (the message has
	// not landed in the replies view yet) — also a fresh conversation.
	opener = isDMChannelID(slackChannel) && (openingTS == "" || openingTS == msg.MessageID)
	bound := a.openingAgentRef(openingText)
	a.bindThreadAgent(msg.ThreadID, bound)
	if bound == "" {
		return a.DefaultAgent, agentSourceDefault, opener
	}
	return bound, agentSourceThread, opener
}

// openingAgentRef extracts the agent binding from a conversation-opening
// message's text: the resolved ref of a complete "/agent <name> <question>"
// prefix, or "" (default agent) for anything else. A name-only or malformed
// prefix never started a conversation, so it binds nothing.
//
// The name MUST come from splitAgentCommand — the same splitter the live
// selection used — never re-derived with different tokenization: live
// selection and recovery must resolve identically for every input, or a
// restart rebinds a conversation to a different agent than the one that
// answered it (a session fork).
func (a *Adapter) openingAgentRef(openingText string) string {
	text := StripMention(openingText)
	cmd := parseCommand(text)
	if cmd == nil || cmd.Name != cmdAgent {
		return ""
	}
	name, question := splitAgentCommand(text)
	if name == "" || question == "" {
		return ""
	}
	ref, ok := a.agentRefFromName(name)
	if !ok {
		return ""
	}
	return ref
}
