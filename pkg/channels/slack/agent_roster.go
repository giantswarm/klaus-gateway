package slack

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// AgentRosterSource lists the agents selectable from Slack, discovered
// dynamically from the kagent controller (never a static gateway config list).
// Implemented by pkg/a2a.Client; nil disables the roster listing.
type AgentRosterSource interface {
	ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error)
}

// agentRosterUnavailable answers the agents command when the roster fetch failed.
const agentRosterUnavailable = "The agents cannot be listed right now. Try again in a moment."

// rosterSignInLead opens every notice that sends an unidentified caller to
// sign in before the roster can be listed: the controller serves the roster to
// a human identity, so a retry cannot help them. Each surface appends its own
// tail (the slash command's slashCommandSignInNotice, the listing's
// agentRosterSignIn), so a wording change lands on all of them.
const rosterSignInLead = "The agents are listed with your permissions, so sign in first: mention the bot with `login`"

// agentRosterSignIn answers the agents command from a caller the gateway
// cannot identify.
const agentRosterSignIn = rosterSignInLead + ", sign in, then send what you typed again."

// agentRosterEmpty answers the agents command when the controller reports no agents.
const agentRosterEmpty = "No agents are installed."

// rosterListTimeout bounds the list-agents call behind a roster listing or a
// selector resolution.
const rosterListTimeout = 5 * time.Second

// rosterListing renders the selectable-agent roster as text: display names
// (the display-name annotation on the AgentTemplate, falling back to the
// technical name), each agent's tenant, and the templates' descriptions. It answers where only text can go (a picker
// notice through a response_url) and is the rows message's fallback text. The
// error is the fetch's own, so a caller can tell a missing identity
// (pkga2a.ErrNoIdentity) from a roster failure.
func (a *Adapter) rosterListing(ctx context.Context) (string, error) {
	agents, err := a.rosterAgents(ctx)
	if err != nil {
		return "", err
	}
	return rosterText(agents), nil
}

func rosterText(agents []pkga2a.AgentInfo) string {
	if len(agents) == 0 {
		return agentRosterEmpty
	}
	var b strings.Builder
	b.WriteString("*Available agents*:")
	for _, ag := range agents {
		b.WriteString("\n• *" + escapeMrkdwn(agentDisplayName(ag)) + "* (" + escapeMrkdwn(agentTenant(ag)) + ")")
		if ag.Description != "" {
			b.WriteString(" — " + escapeMrkdwn(ag.Description))
		}
	}
	return b.String()
}

// The roster message: at most rosterRowsMax agents as rows, each with a Select
// button (action agentSelectAction, value the agent ref) that opens the picker
// with that agent preselected, for the thread the roster was posted in.
const (
	rosterRowsMax     = 8
	agentSelectAction = "agent_select"
	agentSelectLabel  = "Select"
	rosterTitle       = "Agents"
	// rosterDescMax caps a row's description after the first-sentence cut.
	rosterDescMax = 300
)

// postRoster posts the roster as rows in the thread. selectable adds the
// Select buttons; a thread that already has its conversation gets the rows
// without them, since the picker refuses such a thread. A roster that lists no
// agent says so, and one that cannot be listed leaves the reply to the caller,
// which answers the returned error.
func (a *Adapter) postRoster(ctx context.Context, channel, threadID string, selectable bool) error {
	agents, err := a.rosterAgents(ctx)
	client := a.apiClient()
	if err != nil || len(agents) == 0 {
		if err == nil {
			if _, perr := client.postMessage(ctx, channel, agentRosterEmpty, threadID); perr != nil {
				a.Logger.Warn("slack: post roster reply failed", "thread", threadID, "error", perr)
			}
		}
		return err
	}
	fallback := rosterText(agents)
	blocks := a.rosterBlocks(agents, selectable)
	if _, perr := client.postBlocks(ctx, channel, threadID, truncateRunes(fallback, slackSectionTextMax), blocks); perr != nil {
		a.Logger.Warn("slack: post roster failed", "thread", threadID, "error", perr)
	}
	return nil
}

// rosterBlocks renders the roster rows: a header, how many agents there are
// and which one the picker preselects, one row per agent (the default first,
// then A–Z) with its description's first sentence and a Select button, and a
// line naming the agents past rosterRowsMax and the typed way to pick one.
// Without selectable, the rows carry no button and the footer says to start a
// new thread.
func (a *Adapter) rosterBlocks(agents []pkga2a.AgentInfo, selectable bool) []any {
	sorted := slices.Clone(agents)
	isDefault := func(ag pkga2a.AgentInfo) bool { return a.agentInfoRef(ag) == a.DefaultAgent }
	slices.SortStableFunc(sorted, func(x, y pkga2a.AgentInfo) int {
		if dx, dy := isDefault(x), isDefault(y); dx != dy {
			if dx {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(agentDisplayName(x)), strings.ToLower(agentDisplayName(y)))
	})

	summary := countOf(int64(len(sorted)), "agent") + " available"
	if len(sorted) > 0 && isDefault(sorted[0]) {
		summary += " · " + escapeMrkdwn(agentDisplayName(sorted[0])) + " is preselected in the picker"
	}
	blocks := []any{
		map[string]any{bkType: bkHeader, bkText: plainTextObj(rosterTitle)},
		contextBlock(summary),
		map[string]any{bkType: bkDivider},
	}
	shown := sorted[:min(len(sorted), rosterRowsMax)]
	for _, ag := range shown {
		text := "*" + escapeMrkdwn(agentDisplayName(ag)) + "* · " + escapeMrkdwn(agentTenant(ag))
		if isDefault(ag) {
			text += " · default"
		}
		if desc := firstSentence(ag.Description); desc != "" {
			text += "\n" + escapeMrkdwn(truncateRunes(desc, rosterDescMax))
		}
		row := map[string]any{
			bkType: bkSection,
			bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
		}
		if selectable {
			row[bkAccessory] = map[string]any{
				bkType:     bkButton,
				bkText:     plainTextObj(agentSelectLabel),
				bkActionID: agentSelectAction,
				bkValue:    a.agentInfoRef(ag),
			}
		}
		blocks = append(blocks, row)
	}
	footer := "Select starts a conversation with that agent in this thread."
	if !selectable {
		// Only the picker opens a conversation, and this thread has its
		// agent: the slash command opens one on a new root.
		footer = fmt.Sprintf("This thread already has its agent. To talk to another one, start a new conversation with `%s` and pick it there.", a.slashCommand())
	}
	if rest := sorted[len(shown):]; len(rest) > 0 {
		names := make([]string, len(rest))
		for i, ag := range rest {
			names[i] = escapeMrkdwn(agentDisplayName(ag))
		}
		// The agents past the cut have no row, so no Select button reaches
		// them: the pickers are where they can be chosen.
		footer = fmt.Sprintf("%d more: %s — start one of those from the app's slash command, or the ⋯ menu's *Ask an agent here*. ",
			len(rest), strings.Join(names, ", ")) + footer
	}
	return append(blocks, map[string]any{bkType: bkDivider}, contextBlock(truncateRunes(footer, slackSectionTextMax)))
}

// firstSentence is a description cut at its first sentence end (". ", "! ",
// "? " or a line break), with whitespace collapsed; the whole text when it has
// one sentence.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	for i := 0; i+1 < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && s[i+1] == ' ' {
			s = s[:i+1]
			break
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

// displayNameMaxRunes caps a display name at the Slack boundary. Slack
// documents no username limit, so the cap is defensive: the annotation can
// bypass the chart's 63-character schema (a hand-annotated AgentTemplate,
// another chart), and an over-long value must cost the branding, never the reply.
const displayNameMaxRunes = 80

// sanitizeDisplayName makes an annotation value usable as a Slack username:
// control characters become spaces (a multi-line annotation is legal
// Kubernetes), whitespace runs collapse, and the result is capped at
// displayNameMaxRunes. Returns "" when nothing displayable remains, which
// callers treat as an absent annotation.
func sanitizeDisplayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > displayNameMaxRunes {
		s = strings.TrimSpace(string(runes[:displayNameMaxRunes]))
	}
	return s
}

// agentDisplayName is the roster label for ag: the display-name annotation
// when the AgentTemplate carries one, the technical name otherwise. The annotation
// is sanitized here — the one place that knows the value is about to become a
// Slack API parameter — so a hostile or malformed annotation degrades the
// label instead of the message carrying it.
func agentDisplayName(ag pkga2a.AgentInfo) string {
	if name := sanitizeDisplayName(ag.DisplayName); name != "" {
		return name
	}
	return ag.Name
}

// agentTenant is the label of ag's tenant: the platform, or its organization.
func agentTenant(ag pkga2a.AgentInfo) string {
	if ag.Tenant != "" {
		return ag.Tenant
	}
	return pkga2a.PlatformTenant
}

// agentNameFor is the agent's human-facing name on every Slack surface that
// shows one: the username on its own messages and the slash command's root text
// alike, so the two can never disagree. It is the display name the roster
// reports, or agentRef's bare technical name when the roster has nothing for it.
//
// The name is the AgentTemplate's display-name annotation — the same source the
// roster lists — so no surface ever shows a spelling another
// one refuses.
func (a *Adapter) agentNameFor(ctx context.Context, agentRef string) string {
	if name := a.rosterDisplayName(ctx, agentRef); name != "" {
		return name
	}
	return bareAgentName(agentRef)
}

// rosterDisplayName returns the display name the agent had in the last roster
// the gateway fetched (AgentCards.CardIdentity), or "" when there is no card
// resolver or nothing is known for the ref. It needs no caller and decides
// nothing about access. Branding must never block or fail a turn, so every one
// of those is a silent fall-through to the technical name.
func (a *Adapter) rosterDisplayName(ctx context.Context, agentRef string) string {
	if agentRef == "" || a.AgentCards == nil {
		return ""
	}
	name, _ := a.AgentCards.CardIdentity(ctx, agentRef)
	return sanitizeDisplayName(name)
}

// bareAgentName drops any qualifier from agentRef, leaving the Agent
// resource's own name — the spelling the CR carries. The
// last path segment is taken, so the result never contains a slash whatever
// shape an unvalidated ref arrives in. Refs bound from a selection are
// DNS-1123 validated, so this is that name exactly.
func bareAgentName(agentRef string) string {
	if i := strings.LastIndex(agentRef, "/"); i >= 0 {
		return agentRef[i+1:]
	}
	return agentRef
}

// rosterAgents returns the caller's roster, fetched as the caller each time:
// the controller answers each person with the Agents they may use, so one
// person's answer is never served to another.
func (a *Adapter) rosterAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	if a.Roster == nil {
		return nil, fmt.Errorf("slack: no agent roster source configured")
	}
	lctx, cancel := context.WithTimeout(ctx, rosterListTimeout)
	defer cancel()
	return a.Roster.ListAgents(lctx)
}

// refShape renders (namespace, name) in the deployment's ref shape. An empty
// namespace means the served one. An Agent of the served namespace is bare
// under a bare default and qualified under a qualified one, so the refs
// threads already carry keep their spelling (a session's context id hashes
// it). An Agent of any other namespace (an organization's) is always
// namespace/name. Every ref the adapter binds, compares or persists goes
// through here, so one Agent has one spelling.
func (a *Adapter) refShape(namespace, name string) string {
	if namespace == "" {
		namespace = a.servedNamespace()
	}
	if namespace == a.servedNamespace() && a.defaultAgentNamespace() == "" {
		return name
	}
	return namespace + "/" + name
}

// agentInfoRef is the A2A ref for ag in the deployment's ref shape (refShape).
func (a *Adapter) agentInfoRef(ag pkga2a.AgentInfo) string {
	return a.refShape(ag.Namespace, ag.Name)
}

// servedNamespace is the platform tenant's namespace: Namespace, or the
// default agent's namespace when Namespace is unset.
func (a *Adapter) servedNamespace() string {
	if a.Namespace != "" {
		return a.Namespace
	}
	return a.defaultAgentNamespace()
}
