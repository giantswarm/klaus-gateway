package slack

import (
	"context"
	"fmt"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// detailsLevel returns the tool-activity verbosity for a thread. An un-set
// thread resolves to detailsOn (the MVP default).
func (a *Adapter) detailsLevel(threadID string) detailsLevel {
	a.detailsMu.Lock()
	defer a.detailsMu.Unlock()
	return a.details[threadID]
}

// setDetailsLevel records the verbosity for a thread.
func (a *Adapter) setDetailsLevel(threadID string, level detailsLevel) {
	a.detailsMu.Lock()
	defer a.detailsMu.Unlock()
	if a.details == nil {
		a.details = make(map[string]detailsLevel)
	}
	a.details[threadID] = level
}

// recordTurnUsage stores a turn's summed token counts as the thread's last-turn
// usage and adds them to the session total. A zero turn (no usage reported) is
// ignored so it does not clobber a previous turn's figures.
func (a *Adapter) recordTurnUsage(threadID string, turn channels.TurnUsage) {
	if turn == (channels.TurnUsage{}) {
		return
	}
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	if a.lastTurn == nil {
		a.lastTurn = make(map[string]channels.TurnUsage)
		a.sessionTotal = make(map[string]channels.TurnUsage)
	}
	a.lastTurn[threadID] = turn
	total := a.sessionTotal[threadID]
	total.InputTokens += turn.InputTokens
	total.OutputTokens += turn.OutputTokens
	total.TotalTokens += turn.TotalTokens
	a.sessionTotal[threadID] = total
}

// usageReport renders the /usage reply for a thread.
func (a *Adapter) usageReport(ctx context.Context, threadID string) string {
	a.usageMu.Lock()
	last, ok := a.lastTurn[threadID]
	session := a.sessionTotal[threadID]
	a.usageMu.Unlock()
	if !ok {
		return "Token usage not available yet."
	}
	report := fmt.Sprintf("*Token usage*\n• Last turn — %s\n• Session — %s",
		formatUsage(last), formatUsage(session))
	if model := a.agentModelLabel(ctx); model != "" {
		report += "\n• Model — " + model
	}
	return report
}

func formatUsage(u channels.TurnUsage) string {
	return fmt.Sprintf("in %d · out %d · total %d", u.InputTokens, u.OutputTokens, u.TotalTokens)
}

// AgentModelSource resolves the model id and provider behind an agent.
// Implemented by pkg/a2a.AgentsClient against the kagent REST API; empty
// strings with a nil error mean the backend does not expose a model for the
// agent (a BYO runtime).
type AgentModelSource interface {
	AgentModel(ctx context.Context, agentRef string) (model, provider string, err error)
}

// agentModelTTL bounds how long a resolved model label is served from cache.
// The model comes from the agent's CRD spec, which changes rarely.
const agentModelTTL = 10 * time.Minute

// agentModelLabel returns "provider/model" (or just the model when the
// provider is not reported) for the adapter's default agent, or "" when no
// model source is configured, the agent exposes no model, or the lookup fails.
// Results are cached so /usage does not hit the kagent REST API on every call.
func (a *Adapter) agentModelLabel(ctx context.Context) string {
	if a.Models == nil || a.DefaultAgent == "" {
		return ""
	}

	now := time.Now()
	a.modelMu.Lock()
	if e, ok := a.modelCache[a.DefaultAgent]; ok && now.Before(e.expires) {
		a.modelMu.Unlock()
		return e.label
	}
	a.modelMu.Unlock()

	model, provider, err := a.Models.AgentModel(ctx, a.DefaultAgent)
	if err != nil {
		a.Logger.Warn("slack: agent model lookup failed", "agent", a.DefaultAgent, "error", err)
		return ""
	}
	label := model
	if provider != "" && model != "" {
		label = provider + "/" + model
	}

	a.modelMu.Lock()
	if a.modelCache == nil {
		a.modelCache = make(map[string]modelEntry)
	}
	a.modelCache[a.DefaultAgent] = modelEntry{label: label, expires: now.Add(agentModelTTL)}
	a.modelMu.Unlock()
	return label
}

// modelEntry is a cached agent model label with its expiry.
type modelEntry struct {
	label   string
	expires time.Time
}

// sessionChecker is the optional Gateway capability that reports whether a
// thread's kagent session already exists (so a reply resumes it). The Facade
// implements it; adapters degrade gracefully when the gateway does not.
type sessionChecker interface {
	SessionResumable(ctx context.Context, msg channels.InboundMessage) (exists, checked bool)
}

// maybeAnnounceResume runs the resume existence-check at most once per thread
// per process. When the session is confirmed gone it posts the "starting fresh"
// notice; a confirmed-present or indeterminate result stays silent
// (resume-by-default). The check is advisory and never blocks the turn.
func (a *Adapter) maybeAnnounceResume(ctx context.Context, msg channels.InboundMessage, slackChannel string) {
	sc, ok := a.gw.(sessionChecker)
	if !ok {
		return
	}

	a.resumeMu.Lock()
	if a.resumeChecked == nil {
		a.resumeChecked = make(map[string]struct{})
	}
	if _, seen := a.resumeChecked[msg.ThreadID]; seen {
		a.resumeMu.Unlock()
		return
	}
	a.resumeChecked[msg.ThreadID] = struct{}{}
	a.resumeMu.Unlock()

	exists, checked := sc.SessionResumable(ctx, msg)
	if !checked || exists {
		return
	}
	if _, err := a.apiClient().postMessage(ctx, slackChannel, resumeStartingFreshNotice, msg.ThreadID); err != nil {
		a.Logger.Warn("slack: post starting-fresh notice failed", "thread", msg.ThreadID, "error", err)
	}
}
