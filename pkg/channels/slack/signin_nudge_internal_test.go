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
// thread can read may carry it (klaus-gateway#185): the prompt is ephemeral,
// and the notice that anchors it names who the thread waits for, is posted
// once, and is reused by a re-prompt.
func TestPostSignIn_ChannelPromptStaysPrivate(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{} // still unlinked: the convergence check must not drain the anchor

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage)

	require.Equal(t, int32(1), srv.ephemerals.Load(), "the prompt reaches its user ephemerally")
	require.Equal(t, int32(1), srv.posts.Load(), "one thread notice anchors the ephemeral")
	srv.mu.Lock()
	notice, prompt, bodies := srv.postTexts[0], srv.ephemeralTexts[0], strings.Join(srv.postBodies, "\n")
	srv.mu.Unlock()
	require.Equal(t, "Waiting for <@U1> to sign in to Giant Swarm", notice)
	require.NotContains(t, bodies, "example.test/link", "the link must not reach a public message")
	require.Contains(t, prompt, "*Sign in to Giant Swarm*")
	require.Contains(t, prompt, "The link is valid for 15 minutes.")
	require.Contains(t, prompt, signInForMessageLine, "a held message runs after the sign-in")

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage)
	require.Equal(t, int32(1), srv.posts.Load(), "a re-prompt reuses the notice already in the thread")
	require.Zero(t, srv.updates.Load(), "the notice already names the user")
	require.Equal(t, int32(2), srv.ephemerals.Load(), "each re-prompt posts a fresh ephemeral")
}

// A DM thread has one reader, so the prompt stays a real message there: an
// ephemeral would hide nothing and would give up the ts the completed link
// rewrites in place.
func TestPostSignIn_DMPromptStaysAddressable(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{}

	a.postSignIn(t.Context(), "D1", "T1", "U1", false, signInForMessage)

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

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ephemeral: true})
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

// One notice serves the whole thread and names the people it waits for: a
// second unlinked user joins it, a completed link moves a user to "signed in",
// and once nobody waits it names who signed in. Another thread gets its own.
func TestPostSignIn_ThreadNoticeNamesWhoItWaitsFor(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{}
	latest := func() string {
		t.Helper()
		srv.mu.Lock()
		defer srv.mu.Unlock()
		require.NotEmpty(t, srv.updateTexts)
		return srv.updateTexts[len(srv.updateTexts)-1]
	}

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage)
	a.postSignIn(t.Context(), "C1", "T1", "U2", false, signInForMessage)
	require.Equal(t, int32(1), srv.posts.Load(), "a second unlinked user joins the thread's notice")
	require.Equal(t, "Waiting for <@U1> and <@U2> to sign in to Giant Swarm", latest())

	a.updateSignInAnchors(t.Context(), "U1") // U1's link completes
	require.Equal(t, "Waiting for <@U2> to sign in to Giant Swarm", latest())

	a.updateSignInAnchors(t.Context(), "U2")
	require.Equal(t, "<@U1> and <@U2> signed in to Giant Swarm", latest())

	updates := srv.updates.Load()
	a.updateSignInAnchors(t.Context(), "U2")
	require.Equal(t, updates, srv.updates.Load(), "a notice that does not wait for the user is left alone")

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage) // U1 signed out and writes again
	require.Equal(t, int32(1), srv.posts.Load(), "the thread keeps its one notice")
	require.Equal(t, "Waiting for <@U1> to sign in to Giant Swarm", latest())

	a.postSignIn(t.Context(), "C2", "T2", "U1", false, signInForMessage)
	require.Equal(t, int32(2), srv.posts.Load(), "a different thread gets its own notice")
}

// The card's last line follows what asked for it: a held message runs by
// itself, a button click is clicked again, /login needs nothing more.
func TestSignInPromptBody_Trigger(t *testing.T) {
	text := func(trigger signInTrigger) string {
		return signInPromptBody("C1", "T1", "https://example.test/link", false, trigger)[paramText].(string)
	}
	require.True(t, strings.HasSuffix(text(signInForMessage), signInForMessageLine))
	require.True(t, strings.HasSuffix(text(signInForClick), signInForClickLine))
	require.True(t, strings.HasSuffix(text(signInForLogin), "The link is valid for 15 minutes."))

	blocks := signInPromptBody("C1", "T1", "https://example.test/link", true, signInForLogin)[paramBlocks].([]any)
	require.Len(t, blocks, 4, "superseded note, section, button, session hint")
	require.Equal(t, contextBlock(signInLinkSupersededNote), blocks[0])
	require.Equal(t, contextBlock(signInSessionHint), blocks[3])
}

func TestJoinMentions(t *testing.T) {
	require.Equal(t, "<@A>", joinMentions([]string{"A"}))
	require.Equal(t, "<@A> and <@B>", joinMentions([]string{"A", "B"}))
	require.Equal(t, "<@A>, <@B> and <@C>", joinMentions([]string{"A", "B", "C"}))
}

// Slack cannot rewrite or delete an ephemeral, so a channel prompt that
// replaces one whose link expired says so itself; the DM path rewrites the
// dead prompt instead and its fresh prompt stays clean.
func TestMaybePostSignIn_SupersededChannelPromptWarnsOnTheFreshOne(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{}

	staleAt := time.Now().Add(-signInNudgeTTL - time.Minute)
	a.signInPromptedMu.Lock()
	a.signInPrompted = map[string]ttlEntry[signInAnchor]{
		"U1\x00T1": {
			value:   signInAnchor{channel: "C1", ephemeral: true, nudgedAt: staleAt},
			expires: time.Now().Add(pendingTTL),
		},
	}
	a.signInPromptedMu.Unlock()

	a.maybePostSignIn(t.Context(), "C1", "T1", "U1")

	require.Zero(t, srv.updates.Load(), "an ephemeral prompt has no message to rewrite")
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Len(t, srv.ephemeralTexts, 1)
	require.Contains(t, srv.ephemeralTexts[0], signInLinkSupersededNote,
		"the fresh prompt tells the user which button is live")
	require.Contains(t, srv.ephemeralTexts[0], "*Sign in to Giant Swarm*")
}
