package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
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

	a.maybePostSignIn(t.Context(), "D1", "T1", "U1", signInForMessage)
	require.Equal(t, int32(1), srv.posts.Load(), "a fresh prompt posts once the old link expired")

	a.maybePostSignIn(t.Context(), "D1", "T1", "U1", signInForMessage)
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

// responseURLRecorder is a response_url endpoint that records every body and
// answers status.
type responseURLRecorder struct {
	mu     sync.Mutex
	bodies []map[string]any
	status int
}

func newResponseURLRecorder(t *testing.T, status int) (*responseURLRecorder, string) {
	t.Helper()
	r := &responseURLRecorder{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(req.Body).Decode(&v)
		r.mu.Lock()
		r.bodies = append(r.bodies, v)
		r.mu.Unlock()
		w.WriteHeader(r.status)
	}))
	t.Cleanup(srv.Close)
	return r, srv.URL
}

func (r *responseURLRecorder) calls() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.bodies...)
}

func signInClick(user, threadID, responseURL string) interactionPayload {
	var p interactionPayload
	p.Type = payloadTypeBlockActions
	p.User.ID = user
	p.Channel.ID = "C1"
	p.ResponseURL = responseURL
	p.Actions = append(p.Actions, struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	}{ActionID: oboSignIn, Value: threadID})
	return p
}

// A channel prompt whose Sign in click reached this process is replaced through
// the click's response_url when the link completes: the card loses its button
// and no second ephemeral is posted (klaus-gateway#334).
func TestUpdateSignInAnchors_ReplacesTheClickedChannelPrompt(t *testing.T) {
	a, srv := newTestAdapter(t)
	rec, responseURL := newResponseURLRecorder(t, http.StatusOK)

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ephemeral: true})
	a.routeInteraction(t.Context(), signInClick("U1", "T1", responseURL))
	a.updateSignInAnchors(t.Context(), "U1")

	calls := rec.calls()
	require.Len(t, calls, 1, "the prompt is replaced once")
	require.Equal(t, true, calls[0]["replace_original"])
	require.Equal(t, "T1", calls[0]["thread_ts"], "the replacement stays in the thread")
	require.Equal(t, signedInNotice, calls[0]["text"])
	_, hasBlocks := calls[0]["blocks"]
	require.False(t, hasBlocks, "the replacement carries no button")
	require.Zero(t, srv.ephemerals.Load(), "no second ephemeral")
}

// A replace that fails falls back to the separate confirmation, so the person
// still learns the sign-in completed.
func TestUpdateSignInAnchors_FailedReplacePostsTheConfirmation(t *testing.T) {
	a, srv := newTestAdapter(t)
	rec, responseURL := newResponseURLRecorder(t, http.StatusNotFound)

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ephemeral: true})
	a.recordSignInClick("U1", "T1", responseURL)
	a.updateSignInAnchors(t.Context(), "U1")

	require.Len(t, rec.calls(), 1, "the replace was tried")
	require.Equal(t, int32(1), srv.ephemerals.Load(), "the confirmation posts on its own")
}

// Only a live ephemeral anchor under the click's (user, thread) takes the
// response_url: a click never creates an anchor, and a DM prompt keeps its
// in-place rewrite by ts.
func TestRecordSignInClick_OnlyFillsALiveEphemeralAnchor(t *testing.T) {
	a, _ := newTestAdapter(t)
	responseURL := "https://hooks.slack.test/actions/1"

	a.recordSignInClick("U1", "T1", responseURL)
	require.Empty(t, a.takeSignInAnchors("U1"), "a click without a prompt records nothing")

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ephemeral: true})
	a.recordSignInClick("U1", "T2", responseURL)
	a.recordSignInClick("U2", "T1", responseURL)
	anchors := a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.Empty(t, anchors[0].responseURL, "a click in another thread or by another user is not filed")

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "D1", ts: "p.000"})
	a.recordSignInClick("U1", "T1", responseURL)
	anchors = a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.Empty(t, anchors[0].responseURL, "a DM prompt is rewritten by its ts")

	a.recordSignInAnchor("U1", "", signInAnchor{channel: "C1", ephemeral: true})
	a.recordSignInClick("U1", "", responseURL)
	anchors = a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.Equal(t, responseURL, anchors[0].responseURL, "a top-level prompt takes its click")
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
	button := func(blocks []any) map[string]any {
		return blocks[len(blocks)-2].(map[string]any)[bkElements].([]any)[0].(map[string]any)
	}
	require.Equal(t, "T1", button(blocks)[bkValue], "the click names its thread")
	_, hasValue := button(signInPromptBody("C1", "", "https://example.test/link", false, signInForLogin)[paramBlocks].([]any))[bkValue]
	require.False(t, hasValue, "a top-level prompt's button carries no empty value")
	require.Equal(t, contextBlock(signInLinkSupersededNote), blocks[0])
	require.Equal(t, contextBlock(signInSessionHint), blocks[3])
}

// A parked message is replayed once the link completes, so its card says so;
// a bare "login" is parked too but dropped at replay, so its card promises
// nothing.
func TestParkForLogin_TriggerFollowsTheMessage(t *testing.T) {
	for _, tc := range []struct {
		text     string
		promises bool
	}{
		{"why are pods crashlooping?", true},
		{"login", false},
		{"Sign in!", false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			a, srv := newTestAdapter(t)
			a.OBO = deadLinkOBO{}
			a.parkForLogin(t.Context(), channels.InboundMessage{ThreadID: "T1", Text: tc.text}, "C1", "U1")
			srv.mu.Lock()
			defer srv.mu.Unlock()
			require.Len(t, srv.ephemeralTexts, 1)
			if tc.promises {
				require.Contains(t, srv.ephemeralTexts[0], signInForMessageLine)
			} else {
				require.NotContains(t, srv.ephemeralTexts[0], signInForMessageLine)
			}
		})
	}
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

	a.maybePostSignIn(t.Context(), "C1", "T1", "U1", signInForMessage)

	require.Zero(t, srv.updates.Load(), "an ephemeral prompt has no message to rewrite")
	srv.mu.Lock()
	defer srv.mu.Unlock()
	require.Len(t, srv.ephemeralTexts, 1)
	require.Contains(t, srv.ephemeralTexts[0], signInLinkSupersededNote,
		"the fresh prompt tells the user which button is live")
	require.Contains(t, srv.ephemeralTexts[0], "*Sign in to Giant Swarm*")
}
