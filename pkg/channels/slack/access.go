package slack

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

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
	// SetInitiator records userID as the thread initiator when none is set (or
	// the access window closed) and returns the effective initiator. A later
	// caller inside the window never displaces the first.
	SetInitiator(ctx context.Context, channelID, threadID, userID string) string
	// Initiator returns the thread initiator, or "" when none is set or the
	// access window closed.
	Initiator(ctx context.Context, channelID, threadID string) string
	// Allowed reports whether userID may instruct the agent in the thread (the
	// initiator, or a user granted via Grant), inside the access window.
	Allowed(ctx context.Context, channelID, threadID, userID string) bool
	// Grant adds userID to the thread's allowed interactors. Additive.
	Grant(ctx context.Context, channelID, threadID, userID string)
}

// threadAccessTTL bounds how long a thread stays "active" (initiator and
// grants retained) without any interaction. A thread past the TTL needs a
// fresh @-mention to re-engage the bot, so a long-lived pod does not keep
// consuming un-mentioned replies in abandoned threads. Sliding: the window is
// computed from the record's LastSeen, which every handled message refreshes
// via SetInitiator.
const threadAccessTTL = 24 * time.Hour

// recordAccess is AccessPolicy over the thread record. The access window is
// computed from the record's LastSeen: past threadAccessTTL of silence the
// initiator and the grants are void and the next author takes the thread
// over; the agent binding in the same record stays.
type recordAccess struct {
	rec     threadRecorder
	channel string
	now     func() time.Time
	lock    func(channelID, threadID string) *sync.Mutex
}

func (p *recordAccess) open(r channels.ThreadRecord) bool {
	return r.Initiator != "" && p.now().Sub(r.LastSeen) <= threadAccessTTL
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
	if !ok || !p.open(r) {
		t := store.Thread{AgentRef: r.AgentRef, Initiator: userID}
		if err := p.rec.SaveThreadRecord(ctx, p.channel, channelID, threadID, t); err != nil {
			slog.Warn("slack: write thread record failed", "thread", threadID, "error", err)
		}
		return userID
	}
	// A handled message refreshes LastSeen: the window slides.
	if err := p.rec.SaveThreadRecord(ctx, p.channel, channelID, threadID, r.Thread); err != nil {
		slog.Warn("slack: refresh thread record failed", "thread", threadID, "error", err)
	}
	return r.Initiator
}

func (p *recordAccess) Initiator(ctx context.Context, channelID, threadID string) string {
	r, ok, err := p.rec.ThreadRecord(ctx, p.channel, channelID, threadID)
	if err != nil || !ok || !p.open(r) {
		return ""
	}
	return r.Initiator
}

func (p *recordAccess) Allowed(ctx context.Context, channelID, threadID, userID string) bool {
	r, ok, err := p.rec.ThreadRecord(ctx, p.channel, channelID, threadID)
	if err != nil || !ok || !p.open(r) {
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
