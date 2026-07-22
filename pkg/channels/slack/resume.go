package slack

import (
	"context"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

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
