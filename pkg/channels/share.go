package channels

import (
	"context"
	"log/slog"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// withOwnerAuth is withCallerAuth for the calls on a thread's AgentInstance
// that only its creator may make: on a collaborator turn they run under
// OwnerToken, and under the sender's own token when there is none.
func withOwnerAuth(ctx context.Context, msg InboundMessage) context.Context {
	if msg.OwnerToken == "" {
		return withCallerAuth(ctx, msg)
	}
	ctx = pkga2a.WithAgentRef(ctx, msg.AgentRef)
	return pkga2a.WithForwardedToken(ctx, msg.OwnerToken)
}

// withShare adds the thread's AgentInstance share to ctx on a collaborator
// turn, minting it under OwnerToken when the thread holds none for
// instanceID. It returns ErrShareUnavailable when the thread holds none and
// OwnerToken is empty. A share that cannot be minted is logged, and the turn
// goes out under the sender's token alone for the controller to decide.
func (f *Facade) withShare(ctx context.Context, msg InboundMessage, instanceID string) (context.Context, error) {
	if !msg.Collaborator || f.Sealer == nil || instanceID == "" {
		return ctx, nil
	}
	token, err := f.shareFor(ctx, msg, instanceID)
	if err != nil {
		return nil, err
	}
	return pkga2a.WithShareToken(ctx, token), nil
}

// withInstanceAuth adds to ctx what a call on the thread's bound instance
// needs besides the identity withOwnerAuth set: on a collaborator turn
// without the instance creator's token, the share the thread holds for it,
// without minting one. ok is false when that turn holds no usable share: its
// own token cannot reach the instance, so the controller's answer would say
// nothing about the instance.
func (f *Facade) withInstanceAuth(ctx context.Context, msg InboundMessage, entry store.Entry) (_ context.Context, ok bool) {
	if !msg.Collaborator || msg.OwnerToken != "" {
		return ctx, true
	}
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	token, opened := f.openShare(key, entry.Share, entry.AgentInstanceID)
	if !opened {
		return ctx, false
	}
	return pkga2a.WithShareToken(ctx, token), true
}

// creatorID is the channel user whose token withOwnerAuth puts on ctx: the
// person who creates the thread's instance when this turn needs one.
func creatorID(msg InboundMessage) string {
	if msg.OwnerToken != "" {
		return msg.OwnerID
	}
	return msg.SenderID
}

// recordedCreator is the channel user who created entry's instance.
func recordedCreator(entry store.Entry) string {
	if entry.InstanceCreator != "" {
		return entry.InstanceCreator
	}
	return entry.Initiator
}

// shareFor returns the token of the thread's share of instanceID: the one the
// row holds, or a new one minted under OwnerToken and stored sealed. It
// returns "" for an instance the sender created, and when minting fails.
func (f *Facade) shareFor(ctx context.Context, msg InboundMessage, instanceID string) (string, error) {
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		slog.Warn("channels: read the thread's share failed", "thread", msg.ThreadID, "error", err)
		return "", nil
	}
	var held *store.Share
	if ok {
		if entry.AgentInstanceID == instanceID && msg.SenderID != "" && msg.SenderID == entry.InstanceCreator {
			return "", nil
		}
		held = entry.Share
		if token, opened := f.openShare(key, held, instanceID); opened {
			return token, nil
		}
	}
	if msg.OwnerToken == "" {
		slog.Info("channels: collaborator turn refused, the thread holds no share and the instance creator's token is unavailable",
			"thread", msg.ThreadID, "instance", instanceID)
		return "", ErrShareUnavailable
	}
	ownerCtx := withOwnerAuth(ctx, msg)
	share, err := f.Agent.CreateShare(ownerCtx, instanceID)
	if err != nil {
		slog.Warn("channels: share the thread's agent instance failed", "thread", msg.ThreadID, "instance", instanceID, "error", err)
		return "", nil
	}
	sealed, err := f.Sealer.Seal([]byte(share.Token), shareAAD(key, instanceID))
	if err != nil {
		slog.Warn("channels: seal the thread's share failed", "thread", msg.ThreadID, "error", err)
		f.revokeShare(ownerCtx, msg.ThreadID, share.ID)
		return "", nil
	}
	moved, winner := false, ""
	var displaced *store.Share
	if err := f.Routes.Update(ctx, key, func(e *store.Entry, found bool) bool {
		moved, winner, displaced = false, "", nil
		if !found || e.AgentInstanceID != instanceID {
			moved = true
			return false
		}
		if e.Share != nil && (held == nil || e.Share.ID != held.ID) {
			// Another turn stored a share of this instance meanwhile. One this
			// process cannot open is overwritten, and revoked below.
			if token, opened := f.openShare(key, e.Share, instanceID); opened {
				winner = token
				return false
			}
			displaced = e.Share
		}
		e.Share = &store.Share{ID: share.ID, InstanceID: instanceID, Sealed: sealed}
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
		// The thread left instanceID while the share was minted.
		f.revokeShare(ownerCtx, msg.ThreadID, share.ID)
		return "", nil
	}
	if held != nil {
		// A share the row held but could not open (another instance, or sealed
		// under a key this process does not have) is replaced by this one.
		f.revokeShare(ownerCtx, msg.ThreadID, held.ID)
	}
	if displaced != nil {
		f.revokeShare(ownerCtx, msg.ThreadID, displaced.ID)
	}
	slog.Info("channels: shared the thread's agent instance with its collaborators", "record", "instance_shared",
		"thread", msg.ThreadID, "instance", instanceID, "share", share.ID)
	return share.Token, nil
}

// openShare returns the token of share when it is the thread's share of
// instanceID and opens under this process's key.
func (f *Facade) openShare(key store.Key, share *store.Share, instanceID string) (string, bool) {
	if f.Sealer == nil || share == nil || instanceID == "" || share.InstanceID != instanceID {
		return "", false
	}
	token, err := f.Sealer.Open(share.Sealed, shareAAD(key, instanceID))
	if err != nil {
		slog.Warn("channels: the thread's share does not open under this process's key", "thread", key.ThreadID, "share", share.ID, "error", err)
		return "", false
	}
	return string(token), true
}

// revokeLeftShare revokes the share of an instance the thread leaves, created
// by creator. Only the instance's creator may revoke its shares, and
// kagent answers anyone else NotFound, which reads as success: under
// another identity the revoke is skipped and logged instead.
func (f *Facade) revokeLeftShare(ctx context.Context, msg InboundMessage, share *store.Share, creator string) {
	if creator != "" && creatorID(msg) != creator {
		slog.Warn("channels: a share the thread no longer uses stays valid at the controller, this turn does not hold its instance creator's token",
			"thread", msg.ThreadID, "share", share.ID, "instance", share.InstanceID)
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

// shareAAD binds a sealed share token to its thread and instance, so a token
// copied into another row does not open.
func shareAAD(key store.Key, instanceID string) []byte {
	return []byte(key.Channel + "\x00" + key.ChannelID + "\x00" + key.ThreadID + "\x00" + instanceID)
}
