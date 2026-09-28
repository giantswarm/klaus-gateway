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
// instanceID. Without a share the turn goes out under the sender's token
// alone, which the controller refuses on an instance someone else created.
func (f *Facade) withShare(ctx context.Context, msg InboundMessage, instanceID string) context.Context {
	if !msg.Collaborator || f.Sealer == nil || instanceID == "" {
		return ctx
	}
	return pkga2a.WithShareToken(ctx, f.shareFor(ctx, msg, instanceID))
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
// returns "" when neither is possible; the failure is logged, and the turn
// then fails at the controller the way it would without the share.
func (f *Facade) shareFor(ctx context.Context, msg InboundMessage, instanceID string) string {
	key := threadKey(msg.Channel, msg.ChannelID, msg.ThreadID)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		slog.Warn("channels: read the thread's share failed", "thread", msg.ThreadID, "error", err)
		return ""
	}
	var held *store.Share
	if ok {
		held = entry.Share
		if token, opened := f.openShare(key, held, instanceID); opened {
			return token
		}
	}
	if msg.OwnerToken == "" {
		slog.Info("channels: collaborator turn without a share, and the instance creator's token is unavailable",
			"thread", msg.ThreadID, "instance", instanceID)
		return ""
	}
	ownerCtx := withOwnerAuth(ctx, msg)
	share, err := f.Agent.CreateShare(ownerCtx, instanceID)
	if err != nil {
		slog.Warn("channels: share the thread's agent instance failed", "thread", msg.ThreadID, "instance", instanceID, "error", err)
		return ""
	}
	sealed, err := f.Sealer.Seal([]byte(share.Token), shareAAD(key, instanceID))
	if err != nil {
		slog.Warn("channels: seal the thread's share failed", "thread", msg.ThreadID, "error", err)
		f.revokeShare(ownerCtx, msg.ThreadID, share.ID)
		return ""
	}
	stored := false
	if err := f.Routes.Update(ctx, key, func(e *store.Entry, found bool) bool {
		if !found || e.AgentInstanceID != instanceID {
			return false
		}
		e.Share = &store.Share{ID: share.ID, InstanceID: instanceID, Sealed: sealed}
		stored = true
		return true
	}); err != nil {
		slog.Warn("channels: store the thread's share failed", "thread", msg.ThreadID, "error", err)
	}
	if held != nil {
		// A share the row held but could not open (another instance, or sealed
		// under a key this process does not have) is replaced by this one.
		f.revokeShare(ownerCtx, msg.ThreadID, held.ID)
	}
	if !stored {
		// The binding moved while the share was minted: this turn still uses
		// it, but nothing will present it again.
		defer f.revokeShare(context.WithoutCancel(ownerCtx), msg.ThreadID, share.ID)
	}
	slog.Info("channels: shared the thread's agent instance with its collaborators", "record", "instance_shared",
		"thread", msg.ThreadID, "instance", instanceID, "share", share.ID)
	return share.Token
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
