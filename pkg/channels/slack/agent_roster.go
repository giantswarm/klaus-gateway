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

// rosterListTimeout bounds a roster rendering pass: the list-agents call plus
// the per-agent card lookups joined into it.
const rosterListTimeout = 5 * time.Second

// rosterTTL is how long a fetched roster is served from cache. Short, so a
// newly installed agent becomes selectable without a redeploy while repeated
// /agent listings stay off the controller.
const rosterTTL = 30 * time.Second

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
		selector, ref := a.rosterSelector(ag)
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

// rosterSelector is the name a user types to select ag — bare in the default
// agent's namespace, namespace-qualified elsewhere — and the full ref used
// for card lookups.
func (a *Adapter) rosterSelector(ag pkga2a.AgentInfo) (selector, ref string) {
	selector, ref = ag.Name, ag.Name
	if ag.Namespace != "" {
		ref = ag.Namespace + "/" + ag.Name
		if ag.Namespace != a.defaultAgentNamespace() {
			selector = ref
		}
	}
	return selector, ref
}
