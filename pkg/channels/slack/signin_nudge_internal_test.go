package slack

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The sign-in nudge throttle re-arms on signInNudgeTTL (the link URL's state
// lifetime, 15 minutes, matching musterlink's defaultStateTTL) while the
// throttle entry itself lives pendingTTL (24 hours) in its rewrite-anchor
// role. Suppressing on the entry's own expiry would leave the user parked
// behind a button whose URL died 15 minutes in.
func TestShouldPostSignInNudge(t *testing.T) {
	now := time.Now()
	require.Less(t, signInNudgeTTL, pendingTTL,
		"the nudge window must be shorter than the anchor retention it is carved out of")

	require.True(t, shouldPostSignInNudge(ttlEntry[signInAnchor]{}, false, now),
		"no recorded prompt nudges")

	live := ttlEntry[signInAnchor]{
		value:   signInAnchor{ts: "1.1", nudgedAt: now.Add(-signInNudgeTTL + time.Minute)},
		expires: now.Add(pendingTTL),
	}
	require.False(t, shouldPostSignInNudge(live, true, now),
		"a prompt whose link is still valid suppresses the nudge")

	deadLink := ttlEntry[signInAnchor]{
		value:   signInAnchor{ts: "1.1", nudgedAt: now.Add(-signInNudgeTTL)},
		expires: now.Add(pendingTTL),
	}
	require.True(t, shouldPostSignInNudge(deadLink, true, now),
		"a prompt whose link state expired re-nudges even though the anchor entry is retained")

	expired := ttlEntry[signInAnchor]{
		value:   signInAnchor{ts: "1.1", nudgedAt: now},
		expires: now.Add(-time.Minute),
	}
	require.True(t, shouldPostSignInNudge(expired, true, now),
		"an expired entry nudges")
}

// A re-nudge replaces the dead prompt: the new prompt posts, the throttle
// re-arms on the new post, and the recorded anchor points at the new message.
func TestMaybePostSignIn_RepromptsAfterLinkExpiry(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{} // still unlinked: the post-prompt convergence check must not drain the anchor

	staleAt := time.Now().Add(-signInNudgeTTL - time.Minute)
	a.signInPromptedMu.Lock()
	a.signInPrompted = map[string]ttlEntry[signInAnchor]{
		"U1\x00T1": {
			value:   signInAnchor{channel: "C1", ts: "old.000", nudgedAt: staleAt},
			expires: time.Now().Add(pendingTTL),
		},
	}
	a.signInPromptedMu.Unlock()

	a.maybePostSignIn(t.Context(), "C1", "T1", "U1")
	require.Equal(t, int32(1), srv.posts.Load(), "a fresh prompt posts once the old link expired")

	a.maybePostSignIn(t.Context(), "C1", "T1", "U1")
	require.Equal(t, int32(1), srv.posts.Load(), "the throttle re-arms on the fresh prompt")

	anchors := a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.NotEqual(t, "old.000", anchors[0].ts, "the anchor follows the fresh prompt")
}
