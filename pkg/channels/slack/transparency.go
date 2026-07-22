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
	entry, ok := a.details[threadID]
	if !ok {
		return detailsOn
	}
	// Reading refreshes the deadline: a thread in active use never reverts to
	// the default mid-conversation, only idle threads are evicted.
	entry.expires = time.Now().Add(threadStateTTL)
	a.details[threadID] = entry
	return entry.value
}

// setDetailsLevel records the verbosity for a thread.
func (a *Adapter) setDetailsLevel(threadID string, level detailsLevel) {
	now := time.Now()
	a.detailsMu.Lock()
	defer a.detailsMu.Unlock()
	if a.details == nil {
		a.details = make(map[string]ttlEntry[detailsLevel])
	}
	sweepExpired(a.details, now)
	a.details[threadID] = ttlEntry[detailsLevel]{value: level, expires: now.Add(threadStateTTL)}
}

// usageTotals is one scope's accumulated token usage: the most recent turn's
// counts plus the running total across turns.
type usageTotals struct {
	lastTurn channels.TurnUsage
	session  channels.TurnUsage
}

func (u usageTotals) add(turn channels.TurnUsage) usageTotals {
	u.lastTurn = turn
	u.session.InputTokens += turn.InputTokens
	u.session.OutputTokens += turn.OutputTokens
	u.session.TotalTokens += turn.TotalTokens
	return u
}

// recordTurnUsage stores a turn's summed token counts as the thread's last-turn
// usage and adds them to the thread's session total. For a DM the counts are
// additionally aggregated per channel: a top-level `/usage` in a DM keys a
// brand-new thread (its own ts), so the channel aggregate is what makes the
// command answerable there. A zero turn (no usage reported) is ignored so it
// does not clobber a previous turn's figures.
func (a *Adapter) recordTurnUsage(threadID, channelID string, turn channels.TurnUsage) {
	if turn == (channels.TurnUsage{}) {
		return
	}
	now := time.Now()
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	if a.threadUsage == nil {
		a.threadUsage = make(map[string]ttlEntry[usageTotals])
		a.channelUsage = make(map[string]ttlEntry[usageTotals])
	}
	sweepExpired(a.threadUsage, now)
	sweepExpired(a.channelUsage, now)
	expires := now.Add(threadStateTTL)
	a.threadUsage[threadID] = ttlEntry[usageTotals]{value: a.threadUsage[threadID].value.add(turn), expires: expires}
	if isDMChannelID(channelID) {
		a.channelUsage[channelID] = ttlEntry[usageTotals]{value: a.channelUsage[channelID].value.add(turn), expires: expires}
	}
}

const usageGuidance = "No token usage recorded for this thread yet. Run `/usage` as a reply inside the agent's thread."

// usageReport renders the /usage reply. The lookup is thread-first; a miss in
// a DM falls back to the channel's aggregated usage, because a top-level DM
// message carries no thread_ts and so keys a thread no turn ever ran in. A
// miss in a regular channel means the command was typed outside the agent's
// thread, so the reply says where to run it instead of claiming no usage
// exists.
func (a *Adapter) usageReport(ctx context.Context, threadID, channelID string) string {
	a.usageMu.Lock()
	entry, ok := a.threadUsage[threadID]
	if !ok && isDMChannelID(channelID) {
		entry, ok = a.channelUsage[channelID]
	}
	a.usageMu.Unlock()
	if !ok {
		if isDMChannelID(channelID) {
			return "Token usage not available yet."
		}
		return usageGuidance
	}
	report := fmt.Sprintf("*Token usage*\n• Last turn — %s\n• Session — %s",
		formatUsage(entry.value.lastTurn), formatUsage(entry.value.session))
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

// maybeAnnounceResume runs the resume existence-check at most once per thread.
// When the session is confirmed gone it posts the "starting fresh" notice; a
// confirmed-present result stays silent (resume-by-default). Only a conclusive
// check marks the thread as checked: an indeterminate result (transport error,
// REST endpoint unavailable) leaves it unmarked so the next message on the
// thread retries instead of the notice being suppressed forever. The check is
// advisory and never blocks the turn. Turns on a thread are serialized, so the
// check-then-mark window admits no concurrent duplicate.
func (a *Adapter) maybeAnnounceResume(ctx context.Context, msg channels.InboundMessage, slackChannel string) {
	sc, ok := a.gw.(sessionChecker)
	if !ok {
		return
	}

	now := time.Now()
	a.resumeMu.Lock()
	if expiry, seen := a.resumeChecked[msg.ThreadID]; seen && now.Before(expiry) {
		a.resumeMu.Unlock()
		return
	}
	a.resumeMu.Unlock()

	exists, checked := sc.SessionResumable(ctx, msg)
	if !checked {
		return
	}
	// A miss here for a thread that visibly has history means the kagent-side
	// session is gone or is keyed under a different identity (kagent scopes
	// sessions by (contextID, user_id) where user_id derives from the forwarded
	// token subject), so surface the conclusive outcome at info.
	a.Logger.Info("slack: session resume check", "record", "resume_check",
		"thread", msg.ThreadID, "channel_id", msg.ChannelID, "subject", msg.Subject, "exists", exists)

	a.resumeMu.Lock()
	if a.resumeChecked == nil {
		a.resumeChecked = make(map[string]time.Time)
	}
	for threadID, expiry := range a.resumeChecked {
		if now.After(expiry) {
			delete(a.resumeChecked, threadID)
		}
	}
	a.resumeChecked[msg.ThreadID] = now.Add(threadStateTTL)
	a.resumeMu.Unlock()

	if exists {
		return
	}
	if _, err := a.apiClient().postMessage(ctx, slackChannel, resumeStartingFreshNotice, msg.ThreadID); err != nil {
		a.Logger.Warn("slack: post starting-fresh notice failed", "thread", msg.ThreadID, "error", err)
	}
}
