package slack

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// agentDisplayName is the roster label for ag: the display-name annotation
// when the Agent CR carries one, the technical name otherwise.
func agentDisplayName(ag pkga2a.AgentInfo) string {
	if ag.DisplayName != "" {
		return ag.DisplayName
	}
	return ag.Name
}

// rosterAgents returns the roster, served from a brief cache so repeated
// listings stay cheap while a newly installed agent still appears without a
// redeploy. Concurrent cold-cache callers may fetch in parallel (the endpoint
// is idempotent; the last result wins) — no single-flight, deliberately.
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
