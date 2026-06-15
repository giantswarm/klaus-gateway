package channels

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// SynthesizeContextID returns a stable A2A contextID derived from the
// five routing dimensions that uniquely identify a channel conversation
// turn. Length-prefixed encoding prevents collisions between inputs whose
// concatenation would otherwise be identical.
func SynthesizeContextID(channel, channelID, userID, threadID, agentRef string) string {
	h := sha256.New()
	for _, s := range []string{channel, channelID, userID, threadID, agentRef} {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}
