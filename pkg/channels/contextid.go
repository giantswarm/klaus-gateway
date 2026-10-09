package channels

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// SynthesizeContextID returns a stable A2A contextID derived from the channel,
// the channel id, the user, the thread, the agent the conversation is bound
// to and the workspace it chose. Length-prefixed encoding prevents collisions
// between inputs whose concatenation would otherwise be identical.
//
// The userID slot is always "": a thread is shared by its participants, so all
// of them must reach the same conversation. It is a parameter rather than a
// dropped one because the hash is the Session idempotency key of every
// release so far, and every live thread depends on it staying the same —
// removing the slot changes every hash and hands every thread a fresh, empty
// Session. For the same reason the workspace enters the hash only when one is
// chosen: a thread without a workspace, or with an explicit none, keeps the
// key it always had. See the call in Facade.sessionFor.
func SynthesizeContextID(channel, channelID, userID, threadID, agentRef string, workspace *store.WorkspaceChoice) string {
	parts := []string{channel, channelID, userID, threadID, agentRef}
	if workspace.Chosen() {
		parts = append(parts, workspace.Namespace, workspace.Name)
	}
	h := sha256.New()
	for _, s := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}
