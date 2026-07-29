package slack

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// AgentRosterSource lists the agents selectable from Slack, discovered
// dynamically from the kagent controller (never a static gateway config list).
// Implemented by pkg/a2a.KagentClient; nil disables the roster listing.
type AgentRosterSource interface {
	ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error)
}

// agentRosterUnavailable answers a bare /agent when the roster fetch failed.
const agentRosterUnavailable = "_I can't list the available agents right now. Please try again in a moment._"

// agentRosterEmpty answers a bare /agent when the controller reports no agents.
const agentRosterEmpty = "_No agents are installed right now._"

// rosterListTimeout bounds the list-agents call behind a roster listing or a
// selector resolution.
const rosterListTimeout = 5 * time.Second

// rosterTTL is how long a fetched roster is served from cache. Short, so a
// newly installed agent becomes selectable without a redeploy while repeated
// /agent listings stay off the controller.
const rosterTTL = 30 * time.Second

// rosterFailureTTL is how long a failed roster fetch is remembered before the
// controller is asked again. It mirrors the AgentCard client's cardFailureTTL
// and exists for the same reason: the roster is resolved synchronously on the
// dispatch path to brand every turn's first message, so without it every turn
// against an unreachable controller pays the full rosterListTimeout.
//
// Only rosterAgentsBestEffort honours it. Callers whose failure is user-facing
// and blocks the turn — /agent selection and opener re-resolution — keep
// re-attempting, because refusing several messages in a row over one blip is
// worse than paying the timeout.
const rosterFailureTTL = 45 * time.Second

// rosterListing renders the selectable-agent roster: display names (the
// display-name annotation on the Agent CR, falling back to the technical
// name) and the Agent CRs' descriptions. Namespaces are deliberately absent —
// which namespace serves a selection is deployment configuration, not
// something a Slack user picks. ok is false when no roster source is
// configured or the fetch failed.
func (a *Adapter) rosterListing(ctx context.Context) (string, bool) {
	agents, err := a.rosterAgents(ctx)
	if err != nil {
		return "", false
	}
	if len(agents) == 0 {
		return agentRosterEmpty, true
	}
	var b strings.Builder
	b.WriteString("*Available agents* — start a new conversation with `/agent \"<name>\" <question>`:")
	for _, ag := range agents {
		b.WriteString("\n• *" + escapeMrkdwn(agentDisplayName(ag)) + "*")
		if ag.Description != "" {
			b.WriteString(" — " + escapeMrkdwn(ag.Description))
		}
	}
	return b.String(), true
}

// displayNameMaxRunes caps a display name at the Slack boundary. Slack
// documents no username limit, so the cap is defensive: the annotation can
// bypass the chart's 63-character schema (a hand-annotated Agent CR, another
// chart), and an over-long value must cost the branding, never the reply.
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
// when the Agent CR carries one, the technical name otherwise. The annotation
// is sanitized here — the one place that knows the value is about to become a
// Slack API parameter — so a hostile or malformed annotation degrades the
// label instead of the message carrying it.
func agentDisplayName(ag pkga2a.AgentInfo) string {
	if name := sanitizeDisplayName(ag.DisplayName); name != "" {
		return name
	}
	return ag.Name
}

// agentNameFor is the agent's human-facing name on every Slack surface that
// shows one: the username on its own messages and the launch announcement text
// alike, so the two can never disagree. It is the display name the roster
// reports, or agentRef's bare technical name when the roster has nothing for it.
//
// The AgentCard name is deliberately not consulted. kagent generates it from the
// resource name with hyphens replaced by underscores, so it renders a spelling
// that appears in no other surface and that /agent will not accept.
func (a *Adapter) agentNameFor(ctx context.Context, agentRef string) string {
	if name := a.rosterDisplayName(ctx, agentRef); name != "" {
		return name
	}
	return bareAgentName(agentRef)
}

// rosterDisplayName returns the roster's label for agentRef, or "" when no
// roster is configured, the fetch fails, or the roster does not know the ref.
// Branding must never block or fail a turn, so every one of those is a silent
// fall-through to the technical name.
func (a *Adapter) rosterDisplayName(ctx context.Context, agentRef string) string {
	if agentRef == "" {
		return ""
	}
	agents, err := a.rosterAgentsBestEffort(ctx)
	if err != nil {
		return ""
	}
	for _, ag := range agents {
		if a.agentInfoRef(ag) == agentRef {
			return agentDisplayName(ag)
		}
	}
	return ""
}

// bareAgentName drops any qualifier from agentRef, leaving the Agent
// resource's own name — the spelling the CR carries and /agent accepts. The
// last path segment is taken, so the result never contains a slash whatever
// shape an unvalidated ref arrives in. Refs bound from a selection are
// DNS-1123 validated, so this is that name exactly.
func bareAgentName(agentRef string) string {
	if i := strings.LastIndex(agentRef, "/"); i >= 0 {
		return agentRef[i+1:]
	}
	return agentRef
}

// rosterAgents returns the roster, served from a brief cache so repeated
// listings stay cheap while a newly installed agent still appears without a
// redeploy. Concurrent cold-cache callers may fetch in parallel (the endpoint
// is idempotent; the last result wins) — no single-flight, deliberately.
//
// A failure is recorded for rosterFailureTTL but not acted on here: this call
// always re-attempts, so a user-facing selection is never refused on the
// strength of an earlier blip. Branding uses rosterAgentsBestEffort instead.
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
		a.rosterMu.Lock()
		a.rosterFailedUntil = time.Now().Add(rosterFailureTTL)
		a.rosterMu.Unlock()
		return nil, err
	}
	a.rosterMu.Lock()
	a.rosterCached, a.rosterExpires = agents, time.Now().Add(rosterTTL)
	a.rosterFailedUntil = time.Time{}
	a.rosterMu.Unlock()
	return agents, nil
}

// rosterAgentsBestEffort is rosterAgents for callers that only want a nicer
// label and can fall back silently — branding, which runs on every turn's first
// message. Unlike rosterAgents it never waits once a roster has ever been
// fetched: a stale cache is served as the last known good roster and refreshed
// in the background (one refresh in flight at a time), so branding is not
// latency-bearing and a transient fetch failure cannot flap an agent's name
// mid-conversation. The negative cache gates only the cold path and the
// background refresh, so an unreachable controller costs one rosterListTimeout
// per rosterFailureTTL rather than one per turn.
func (a *Adapter) rosterAgentsBestEffort(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	if a.Roster == nil {
		return nil, fmt.Errorf("slack: no agent roster source configured")
	}
	now := time.Now()
	a.rosterMu.Lock()
	cached := a.rosterCached
	if cached != nil {
		refresh := !now.Before(a.rosterExpires) && !a.rosterRefreshing && !now.Before(a.rosterFailedUntil)
		if refresh {
			a.rosterRefreshing = true
		}
		a.rosterMu.Unlock()
		if refresh {
			a.background(func(bctx context.Context) {
				defer func() {
					a.rosterMu.Lock()
					a.rosterRefreshing = false
					a.rosterMu.Unlock()
				}()
				if _, err := a.rosterAgents(bctx); err != nil {
					a.Logger.Warn("slack: background roster refresh failed, branding keeps the last known roster", "error", err)
				}
			})
		}
		return cached, nil
	}
	failedUntil := a.rosterFailedUntil
	a.rosterMu.Unlock()
	if now.Before(failedUntil) {
		return nil, fmt.Errorf("slack: recent roster fetch failure, not retrying before %s", failedUntil.Format(time.RFC3339))
	}
	return a.rosterAgents(ctx)
}

// agentRefsForSelector resolves a quoted /agent selector against the roster,
// matching display names and technical names case-insensitively with
// whitespace runs collapsed. Both kinds match in one pass — no precedence — so
// a selector naming two different agents is reported as ambiguous (fail-stop)
// instead of quietly resolved by a tie-break rule that a later cluster change
// could flip to a different agent. Refs are deduped: matching one agent by
// both its display and technical name is a single match.
func (a *Adapter) agentRefsForSelector(ctx context.Context, selector string) ([]string, error) {
	want := foldAgentSelector(selector)
	if want == "" {
		return nil, nil
	}
	agents, err := a.rosterAgents(ctx)
	if err != nil {
		return nil, err
	}
	var refs []string
	seen := make(map[string]bool)
	for _, ag := range agents {
		if foldAgentSelector(ag.DisplayName) != want && foldAgentSelector(ag.Name) != want {
			continue
		}
		ref := a.agentInfoRef(ag)
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// agentInfoRef is the A2A ref for ag under the deployment's ref shape: bare
// when the default agent is bare (the namespace already lives in the
// configured A2A base URL), namespace-qualified when the default is qualified.
func (a *Adapter) agentInfoRef(ag pkga2a.AgentInfo) string {
	if a.defaultAgentNamespace() == "" || ag.Namespace == "" {
		return ag.Name
	}
	return ag.Namespace + "/" + ag.Name
}

// foldAgentSelector normalizes a selector or agent name for matching:
// lowercased, outer whitespace dropped, internal runs collapsed to one space.
func foldAgentSelector(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
