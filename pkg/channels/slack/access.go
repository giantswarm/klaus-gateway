package slack

import (
	"context"
	"log/slog"
	"slices"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// AccessPolicy decides who may instruct the agent in a thread. Reading a thread
// is never gated; the policy governs instructing only. A thread has one
// initiator (the user whose mention launched it) who may always instruct;
// additional users are granted on the fly once the initiator approves them.
// The state lives in the thread's row in the routing store, so on a persistent
// store it survives a gateway restart.
type AccessPolicy interface {
	// SetInitiator records userID as the thread initiator when the thread has
	// none — a new thread, or one the store has forgotten — and returns the
	// effective initiator. A later caller never displaces the first.
	SetInitiator(ctx context.Context, channelID, threadID, userID string) string
	// Initiator returns the thread initiator, or "" when the thread has none.
	Initiator(ctx context.Context, channelID, threadID string) string
	// Allowed reports whether userID may instruct the agent in the thread: the
	// initiator, or a user granted via Grant.
	Allowed(ctx context.Context, channelID, threadID, userID string) bool
	// Grant adds userID to the thread's allowed interactors. Additive.
	Grant(ctx context.Context, channelID, threadID, userID string)
}

// recordAccess is AccessPolicy over the thread's row in the routing store. The
// initiator and the grants live exactly as long as the row does: its lifetime
// is the store's (--thread-ttl), refreshed by every turn. A thread the gateway
// has forgotten has no row at all, so the next mentioner becomes its initiator.
// Every write goes through the store's per-key update, so a grant does not
// erase what the turn wrote on the same row.
type recordAccess struct {
	rec     channels.Gateway
	channel string
}

func (p *recordAccess) SetInitiator(ctx context.Context, channelID, threadID, userID string) string {
	initiator := userID
	err := p.rec.UpdateThreadRecord(ctx, p.channel, channelID, threadID, func(e *store.Entry, _ bool) bool {
		if e.Initiator == "" {
			e.Initiator = userID
			return true
		}
		// Nothing to write: the turn that follows refreshes the row's lifetime,
		// so a stranger's parked message costs no write and keeps nothing alive.
		initiator = e.Initiator
		return false
	})
	if err != nil {
		// The author comes back as initiator while Allowed, reading the same
		// failing store, denies them: the dispatcher reads that pair as a store
		// outage and stops the turn with a notice instead of parking it.
		slog.Warn("slack: write thread record failed", "thread", threadID, "error", err)
		return userID
	}
	return initiator
}

func (p *recordAccess) Initiator(ctx context.Context, channelID, threadID string) string {
	e, ok, err := p.rec.ThreadRecord(ctx, p.channel, channelID, threadID)
	if err != nil || !ok {
		return ""
	}
	return e.Initiator
}

func (p *recordAccess) Allowed(ctx context.Context, channelID, threadID, userID string) bool {
	e, ok, err := p.rec.ThreadRecord(ctx, p.channel, channelID, threadID)
	// A thread with no initiator has no session to answer to.
	if err != nil || !ok || e.Initiator == "" {
		return false
	}
	return e.Initiator == userID || slices.Contains(e.Granted, userID)
}

func (p *recordAccess) Grant(ctx context.Context, channelID, threadID, userID string) {
	err := p.rec.UpdateThreadRecord(ctx, p.channel, channelID, threadID, func(e *store.Entry, found bool) bool {
		if !found {
			return false
		}
		if !slices.Contains(e.Granted, userID) {
			e.Granted = append(e.Granted, userID)
		}
		return true
	})
	if err != nil {
		slog.Warn("slack: write grant failed", "thread", threadID, "user", userID, "error", err)
	}
}
