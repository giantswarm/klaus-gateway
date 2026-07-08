package slack

import (
	"context"
	"fmt"

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
func (a *Adapter) usageReport(threadID string) string {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	last, ok := a.lastTurn[threadID]
	if !ok {
		return "Token usage not available yet."
	}
	return fmt.Sprintf("*Token usage*\n• Last turn — %s\n• Session — %s",
		formatUsage(last), formatUsage(a.sessionTotal[threadID]))
}

func formatUsage(u channels.TurnUsage) string {
	return fmt.Sprintf("in %d · out %d · total %d", u.InputTokens, u.OutputTokens, u.TotalTokens)
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
