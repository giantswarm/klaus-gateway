package slack

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// AccessPolicy decides who may instruct the agent in a thread. Reading a thread
// is never gated; the policy governs instructing only. A thread has one
// initiator (the user whose mention launched it) who may always instruct;
// additional users are granted on the fly once the initiator approves them.
// The state lives in the thread's record in the routing store, so on a
// persistent store it survives a gateway restart.
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

// recordAccess is AccessPolicy over the thread record. The initiator and the
// grants live exactly as long as the record does: its lifetime is the store's
// (--thread-ttl), refreshed by every handled message. A thread the gateway has
// forgotten has no record at all, so the next mentioner becomes its initiator.
type recordAccess struct {
	rec     threadRecorder
	channel string
	lock    func(channelID, threadID string) *sync.Mutex
}

// active reports whether the thread has an initiator to answer to.
func (p *recordAccess) active(r channels.ThreadRecord) bool {
	return r.Initiator != ""
}

func (p *recordAccess) SetInitiator(ctx context.Context, channelID, threadID, userID string) string {
	mu := p.lock(channelID, threadID)
	mu.Lock()
	defer mu.Unlock()
	r, ok, err := p.rec.ThreadRecord(ctx, p.channel, channelID, threadID)
	if err != nil {
		slog.Warn("slack: read thread record failed, treating the author as initiator", "thread", threadID, "error", err)
		return userID
	}
	if !ok || r.Initiator == "" {
		t := store.Thread{AgentRef: r.AgentRef, Initiator: userID}
		if err := p.rec.SaveThreadRecord(ctx, p.channel, channelID, threadID, t); err != nil {
			slog.Warn("slack: write thread record failed", "thread", threadID, "error", err)
		}
		return userID
	}
	// A handled message refreshes LastSeen: the thread's lifetime slides.
	if err := p.rec.SaveThreadRecord(ctx, p.channel, channelID, threadID, r.Thread); err != nil {
		slog.Warn("slack: refresh thread record failed", "thread", threadID, "error", err)
	}
	return r.Initiator
}

func (p *recordAccess) Initiator(ctx context.Context, channelID, threadID string) string {
	r, ok, err := p.rec.ThreadRecord(ctx, p.channel, channelID, threadID)
	if err != nil || !ok || !p.active(r) {
		return ""
	}
	return r.Initiator
}

func (p *recordAccess) Allowed(ctx context.Context, channelID, threadID, userID string) bool {
	r, ok, err := p.rec.ThreadRecord(ctx, p.channel, channelID, threadID)
	if err != nil || !ok || !p.active(r) {
		return false
	}
	return r.Initiator == userID || slices.Contains(r.Granted, userID)
}

func (p *recordAccess) Grant(ctx context.Context, channelID, threadID, userID string) {
	mu := p.lock(channelID, threadID)
	mu.Lock()
	defer mu.Unlock()
	r, ok, err := p.rec.ThreadRecord(ctx, p.channel, channelID, threadID)
	if err != nil || !ok {
		return
	}
	if !slices.Contains(r.Granted, userID) {
		r.Granted = append(r.Granted, userID)
	}
	if err := p.rec.SaveThreadRecord(ctx, p.channel, channelID, threadID, r.Thread); err != nil {
		slog.Warn("slack: write grant failed", "thread", threadID, "user", userID, "error", err)
	}
}
