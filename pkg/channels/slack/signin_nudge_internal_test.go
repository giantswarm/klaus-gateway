package slack

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The sign-in nudge throttle re-arms on signInNudgeTTL (the link URL's state
// lifetime, musterlink.DefaultStateTTL) while the
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
// Driven on a DM, the surface whose prompt carries an addressable ts.
func TestMaybePostSignIn_RepromptsAfterLinkExpiry(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{} // still unlinked: the post-prompt convergence check must not drain the anchor

	staleAt := time.Now().Add(-signInNudgeTTL - time.Minute)
	a.signInPromptedMu.Lock()
	a.signInPrompted = map[string]ttlEntry[signInAnchor]{
		"U1\x00T1": {
			value:   signInAnchor{channel: "D1", ts: "old.000", nudgedAt: staleAt},
			expires: time.Now().Add(pendingTTL),
		},
	}
	a.signInPromptedMu.Unlock()

	a.maybePostSignIn(t.Context(), "D1", "T1", "U1")
	require.Equal(t, int32(1), srv.posts.Load(), "a fresh prompt posts once the old link expired")

	a.maybePostSignIn(t.Context(), "D1", "T1", "U1")
	require.Equal(t, int32(1), srv.posts.Load(), "the throttle re-arms on the fresh prompt")

	anchors := a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.NotEqual(t, "old.000", anchors[0].ts, "the anchor follows the fresh prompt")
}

// In a channel the sign-in link is minted for one identity, so no message the
// thread can read may carry it or name its user (klaus-gateway#185): the prompt
// is ephemeral, and the notice that anchors it is posted once and reused by a
// re-prompt.
func TestPostSignIn_ChannelPromptStaysPrivate(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{} // still unlinked: the convergence check must not drain the anchor

	a.postSignIn(t.Context(), "C1", "T1", "U1")

	require.Equal(t, int32(1), srv.ephemerals.Load(), "the prompt reaches its user ephemerally")
	require.Equal(t, int32(1), srv.posts.Load(), "one thread notice anchors the ephemeral")
	srv.mu.Lock()
	notice, prompt, bodies := srv.postTexts[0], srv.ephemeralTexts[0], strings.Join(srv.postBodies, "\n")
	srv.mu.Unlock()
	require.Equal(t, signInThreadNotice, notice)
	require.NotContains(t, notice, "U1", "the public notice must not name the prompted user")
	require.NotContains(t, bodies, "example.test/link", "the link must not reach a public message")
	require.Contains(t, prompt, signInPromptText)

	a.postSignIn(t.Context(), "C1", "T1", "U1")
	require.Equal(t, int32(1), srv.posts.Load(), "a re-prompt reuses the notice already in the thread")
	require.Equal(t, int32(2), srv.ephemerals.Load(), "each re-prompt posts a fresh ephemeral")
}

// A DM thread has one reader, so the prompt stays a real message there: an
// ephemeral would hide nothing and would give up the ts the completed link
// rewrites in place.
func TestPostSignIn_DMPromptStaysAddressable(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{}

	a.postSignIn(t.Context(), "D1", "T1", "U1")

	require.Equal(t, int32(1), srv.posts.Load(), "the DM prompt is a real message")
	require.Zero(t, srv.ephemerals.Load(), "nothing is hidden in a DM")
	anchors := a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.Equal(t, "1234.5678", anchors[0].ts, "the DM prompt stays addressable for the rewrite")
}

// The completed link is confirmed on the surface that carried the prompt: a
// rewrite in a DM, a fresh ephemeral to the same user in a channel (an
// ephemeral has no ts to rewrite).
func TestUpdateSignInAnchors_ConfirmsPerSurface(t *testing.T) {
	a, srv := newTestAdapter(t)

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", noticeTS: "n.000", ephemeral: true})
	a.updateSignInAnchors(t.Context(), "U1")
	require.Equal(t, int32(1), srv.ephemerals.Load(), "a channel prompt is confirmed privately")
	require.Zero(t, srv.updates.Load(), "an ephemeral prompt has no message to rewrite")
	srv.mu.Lock()
	require.Contains(t, srv.ephemeralTexts[0], "Signed in")
	srv.mu.Unlock()

	a.recordSignInAnchor("U2", "T2", signInAnchor{channel: "D2", ts: "p.000"})
	a.updateSignInAnchors(t.Context(), "U2")
	require.Equal(t, int32(1), srv.updates.Load(), "a DM prompt is rewritten in place")
	require.Equal(t, int32(1), srv.ephemerals.Load(), "the DM rewrite posts nothing new")
}
