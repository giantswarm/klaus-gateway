package channels

import (
	"context"
	"log/slog"
	"time"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// withOwnerAuth is withCallerAuth for the calls on a thread's Session
// that only its creator may make: on a collaborator turn they run under
// OwnerToken, and under the sender's own token when there is none.
func withOwnerAuth(ctx context.Context, msg InboundMessage) context.Context {
	if msg.OwnerToken == "" {
		return withCallerAuth(ctx, msg)
	}
	ctx = pkga2a.WithAgentRef(ctx, msg.AgentRef)
	return pkga2a.WithForwardedToken(ctx, msg.OwnerToken)
}

// withShare adds the thread's Session share to ctx on a collaborator
// turn, minting it under OwnerToken when the thread holds none for
// sessionID. It returns ErrShareUnavailable when the thread holds none and
// OwnerToken is empty. A share that cannot be minted is logged, and the turn
// goes out under the sender's token alone for the controller to decide.
func (f *Facade) withShare(ctx context.Context, msg InboundMessage, sessionID string) (context.Context, error) {
	if !msg.Collaborator || f.Sealer == nil || sessionID == "" {
		return ctx, nil
	}
	token, err := f.shareFor(ctx, msg, sessionID)
	if err != nil {
		return nil, err
	}
	return pkga2a.WithShareToken(ctx, token), nil
}

// withSessionAuth adds to ctx what a call on the thread's bound session
// needs besides the identity withOwnerAuth set: on a collaborator turn
// without the session creator's token, the share the thread holds for it,
// without minting one. ok is false when that turn holds no usable share: its
// own token cannot reach the session, so the controller's answer would say
// nothing about the session.
func (f *Facade) withSessionAuth(ctx context.Context, msg InboundMessage, entry store.Entry) (_ context.Context, ok bool) {
	if !msg.Collaborator || msg.OwnerToken != "" {
		return ctx, true
	}
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	token, opened := f.openShare(key, entry.Share, entry.AgentInstanceID, f.shareExpiryMargin())
	if !opened {
		return ctx, false
	}
	return pkga2a.WithShareToken(ctx, token), true
}

// creatorID is the channel user whose token withOwnerAuth puts on ctx: the
// person who creates the thread's session when this turn needs one.
func creatorID(msg InboundMessage) string {
	if msg.OwnerToken != "" {
		return msg.OwnerID
	}
	return msg.SenderID
}

// recordedCreator is the channel user who created entry's session.
func recordedCreator(entry store.Entry) string {
	if entry.InstanceCreator != "" {
		return entry.InstanceCreator
	}
	return entry.Initiator
}

// shareFor returns the token of the thread's share of sessionID: the one the
// row holds, or a new one minted under OwnerToken and stored sealed. It
// returns "" for a session the sender created, and when minting fails.
func (f *Facade) shareFor(ctx context.Context, msg InboundMessage, sessionID string) (string, error) {
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		slog.Warn("channels: read the thread's share failed", "thread", msg.ThreadID, "error", err)
		return "", nil
	}
	var held *store.Share
	if ok {
		if entry.AgentInstanceID == sessionID && msg.SenderID != "" && msg.SenderID == entry.InstanceCreator {
			return "", nil
		}
		held = entry.Share
		// Without the creator's token no replacement can be minted, so a
		// share is used until shortly before it expires.
		margin := f.shareExpiryMargin()
		if msg.OwnerToken != "" {
			margin = f.shareRenewal()
		}
		if token, opened := f.openShare(key, held, sessionID, margin); opened {
			return token, nil
		}
	}
	if msg.OwnerToken == "" {
		slog.Info("channels: collaborator turn refused, the thread holds no share and the session creator's token is unavailable",
			"thread", msg.ThreadID, "session", sessionID)
		return "", ErrShareUnavailable
	}
	ownerCtx := withOwnerAuth(ctx, msg)
	share, err := f.Agent.CreateShare(ownerCtx, sessionID, f.ThreadTTL)
	if err != nil {
		slog.Warn("channels: share the thread's session failed", "thread", msg.ThreadID, "session", sessionID, "error", err)
		return "", nil
	}
	sealed, err := f.Sealer.Seal([]byte(share.Token), shareAAD(key, sessionID))
	if err != nil {
		slog.Warn("channels: seal the thread's share failed", "thread", msg.ThreadID, "error", err)
		f.revokeShare(ownerCtx, msg.ThreadID, share.ID)
		return "", nil
	}
	moved, winner := false, ""
	var displaced *store.Share
	if err := f.Routes.Update(ctx, key, func(e *store.Entry, found bool) bool {
		moved, winner, displaced = false, "", nil
		if !found || e.AgentInstanceID != sessionID {
			moved = true
			return false
		}
		if e.Share != nil && (held == nil || e.Share.ID != held.ID) {
			// Another turn stored a share of this session meanwhile. One this
			// process cannot open is overwritten, and revoked below.
			if token, opened := f.openShare(key, e.Share, sessionID, 0); opened {
				winner = token
				return false
			}
			displaced = e.Share
		}
		e.Share = &store.Share{ID: share.ID, InstanceID: sessionID, Sealed: sealed, ExpiresAt: share.ExpiresAt}
		return true
	}); err != nil {
		// The turn still uses the share; the next one mints another.
		slog.Warn("channels: store the thread's share failed", "thread", msg.ThreadID, "error", err)
		displaced = nil
	}
	if winner != "" {
		f.revokeShare(ownerCtx, msg.ThreadID, share.ID)
		return winner, nil
	}
	if moved {
		// The thread left sessionID while the share was minted.
		f.revokeShare(ownerCtx, msg.ThreadID, share.ID)
		return "", nil
	}
	if held != nil {
		// A share the row held but could not open (another session, sealed
		// under a key this process does not have, or expired) is revoked. One
		// replaced only for renewal stays valid until its expiry: a turn
		// already running on it keeps working.
		if _, valid := f.openShare(key, held, sessionID, 0); !valid {
			f.revokeShare(ownerCtx, msg.ThreadID, held.ID)
		}
	}
	if displaced != nil {
		f.revokeShare(ownerCtx, msg.ThreadID, displaced.ID)
	}
	slog.Info("channels: shared the thread's session with its collaborators", "record", "session_shared",
		"thread", msg.ThreadID, "session", sessionID, "share", share.ID)
	return share.Token, nil
}

// shareRenewal is how long before its expiry a share is replaced when the
// session's creator can mint the next one: an hour, or half the thread
// lifetime when that is shorter, so a fresh share is never due at once.
func (f *Facade) shareRenewal() time.Duration {
	if f.ThreadTTL > 0 {
		return min(time.Hour, f.ThreadTTL/2)
	}
	return time.Hour
}

// shareExpiryMargin is how long a share must still be valid for a turn to
// start on it when no replacement can be minted: kagent checks the share on
// every call, and a turn's later calls (a stop, the final read, a
// resubscription after a restart) must not outlive it, clock skew between the
// gateway and the controller's database included. Five minutes, or the
// renewal window when that is shorter.
func (f *Facade) shareExpiryMargin() time.Duration {
	return min(5*time.Minute, f.shareRenewal())
}

// openShare returns the token of share when it is the thread's share of
// sessionID, grants access for at least margin more, and opens under this
// process's key.
func (f *Facade) openShare(key store.Key, share *store.Share, sessionID string, margin time.Duration) (string, bool) {
	if f.Sealer == nil || share == nil || sessionID == "" || share.InstanceID != sessionID {
		return "", false
	}
	if !share.ExpiresAt.IsZero() && !f.clock().Add(margin).Before(share.ExpiresAt) {
		slog.Info("channels: the thread's share is expired or about to", "thread", key.ThreadID, "share", share.ID, "expires_at", share.ExpiresAt)
		return "", false
	}
	token, err := f.Sealer.Open(share.Sealed, shareAAD(key, sessionID))
	if err != nil {
		slog.Warn("channels: the thread's share does not open under this process's key", "thread", key.ThreadID, "share", share.ID, "error", err)
		return "", false
	}
	return string(token), true
}

// revokeLeftShare revokes the share of a session the thread leaves, created
// by creator. Only the session's creator may revoke its shares, and
// kagent answers anyone else NotFound, which reads as success: under
// another identity the revoke is skipped and logged instead.
func (f *Facade) revokeLeftShare(ctx context.Context, msg InboundMessage, share *store.Share, creator string) {
	if creator != "" && creatorID(msg) != creator {
		slog.Warn("channels: a share the thread no longer uses stays valid at the controller, this turn does not hold its session creator's token",
			"thread", msg.ThreadID, "share", share.ID, "session", share.InstanceID)
		return
	}
	f.revokeShare(ctx, msg.ThreadID, share.ID)
}

// revokeShare revokes a share the thread no longer uses. Best effort: a
// failure is logged, and the share stays valid at the controller.
func (f *Facade) revokeShare(ctx context.Context, threadID, shareID string) {
	if shareID == "" {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindingWriteTimeout)
	defer cancel()
	if err := f.Agent.RevokeShare(rctx, shareID); err != nil {
		slog.Warn("channels: revoke a share the thread no longer uses failed", "thread", threadID, "share", shareID, "error", err)
		return
	}
	slog.Info("channels: revoked a share the thread no longer uses", "record", "share_revoked", "thread", threadID, "share", shareID)
}

// shareAAD binds a sealed share token to its thread and session, so a token
// copied into another row does not open.
func shareAAD(key store.Key, sessionID string) []byte {
	return []byte(key.Channel + "\x00" + key.ChannelID + "\x00" + key.ThreadID + "\x00" + sessionID)
}
