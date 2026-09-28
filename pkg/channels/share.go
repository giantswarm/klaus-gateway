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

// withStoredShare adds the share the thread already holds for instanceID to
// ctx on a collaborator turn, without minting one.
func (f *Facade) withStoredShare(ctx context.Context, msg InboundMessage, entry store.Entry) context.Context {
	if !msg.Collaborator || f.Sealer == nil {
		return ctx
	}
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	token, ok := f.openShare(key, entry.Share, entry.AgentInstanceID)
	if !ok {
		return ctx
	}
	return pkga2a.WithShareToken(ctx, token)
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
		if entry.SenderCreated && entry.AgentInstanceID == instanceID {
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
	if err := f.Routes.Update(ctx, key, func(e *store.Entry, found bool) bool {
		if !found || e.AgentInstanceID != instanceID {
			moved = true
			return false
		}
		if e.Share != nil && (held == nil || e.Share.ID != held.ID) {
			// Another turn stored a share of this instance meanwhile.
			if token, opened := f.openShare(key, e.Share, instanceID); opened {
				winner = token
				return false
			}
		}
		e.Share = &store.Share{ID: share.ID, InstanceID: instanceID, Sealed: sealed}
		return true
	}); err != nil {
		// The turn still uses the share; the next one mints another.
		slog.Warn("channels: store the thread's share failed", "thread", msg.ThreadID, "error", err)
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
