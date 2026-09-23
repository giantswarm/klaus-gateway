package channels

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// SynthesizeContextID returns a stable A2A contextID derived from the channel,
// the channel id, the user, the thread and the agent the conversation is bound
// to. Length-prefixed encoding prevents collisions between inputs whose
// concatenation would otherwise be identical.
//
// The userID slot is always "": a thread is shared by its participants, so all
// of them must reach the same conversation. It is a parameter rather than a
// dropped one because the hash is the AgentInstance idempotency key of every
// release so far, and every live thread depends on it staying the same —
// removing the slot changes every hash and hands every thread a fresh, empty
// AgentInstance. See the call in Facade.instanceFor.
func SynthesizeContextID(channel, channelID, userID, threadID, agentRef string) string {
	h := sha256.New()
	for _, s := range []string{channel, channelID, userID, threadID, agentRef} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}
