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

	burst := ttlEntry[signInAnchor]{
		value:   signInAnchor{channel: "C1", ephemeral: true, nudgedAt: now.Add(-signInReissueAfter + time.Second)},
		expires: now.Add(pendingTTL),
	}
	require.False(t, shouldPostSignInNudge(burst, true, now),
		"a burst of messages right after a channel prompt prompts once")

	later := burst
	later.value.nudgedAt = now.Add(-signInReissueAfter)
	require.True(t, shouldPostSignInNudge(later, true, now),
		"a later message re-issues a channel prompt, which an ephemeral cannot keep")

	dmLater := ttlEntry[signInAnchor]{
		value:   signInAnchor{channel: "D1", ts: "1.1", nudgedAt: now.Add(-signInReissueAfter)},
		expires: now.Add(pendingTTL),
	}
	require.False(t, shouldPostSignInNudge(dmLater, true, now),
		"a DM prompt is a message that stays, so its live link is not re-issued")
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
	require.Equal(t, "Waiting for <@U1> to sign in to Giant Swarm"+signInWaitingDMHint, notice)
	require.NotContains(t, bodies, "example.test/link", "the link must not reach a public message")
	require.Contains(t, prompt, "*Sign in to Giant Swarm*")
	require.Contains(t, prompt, "The link is valid for 15 minutes.")
	require.Contains(t, prompt, signInForMessageLine, "a held message runs after the sign-in")

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage)
	require.Equal(t, int32(1), srv.posts.Load(), "a re-prompt reuses the notice already in the thread")
	require.Equal(t, int32(1), srv.updates.Load(), "the notice already names the user; only the DM card is refreshed")
	require.Equal(t, int32(2), srv.ephemerals.Load(), "each re-prompt posts a fresh ephemeral")
	require.Equal(t, int32(1), srv.dms.Load(), "the DM holds one card")
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

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ephemeral: true, promptID: "P1"})
	a.routeInteraction(t.Context(), signInClick("U1", "T1|P1", responseURL))
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

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ephemeral: true, promptID: "P1"})
	a.recordSignInClick("U1", "T1|P1", responseURL)
	a.updateSignInAnchors(t.Context(), "U1")

	require.Len(t, rec.calls(), 1, "the replace was tried")
	require.Equal(t, int32(1), srv.ephemerals.Load(), "the confirmation posts on its own")
}

// Only the live ephemeral anchor of the clicked prompt takes the response_url:
// a click never creates an anchor, a click on an older card in the thread
// cannot take the current card's handle, and a DM prompt keeps its in-place
// rewrite by ts.
func TestRecordSignInClick_OnlyFillsTheClickedPromptsAnchor(t *testing.T) {
	a, _ := newTestAdapter(t)
	responseURL := "https://hooks.slack.test/actions/1"

	a.recordSignInClick("U1", "T1|P1", responseURL)
	require.Empty(t, a.takeSignInAnchors("U1"), "a click without a prompt records nothing")

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ephemeral: true, promptID: "P1"})
	a.recordSignInClick("U1", "T2|P1", responseURL)
	a.recordSignInClick("U2", "T1|P1", responseURL)
	a.recordSignInClick("U1", "T1", responseURL)
	anchors := a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.Empty(t, anchors[0].responseURL, "a click in another thread, by another user, or without a prompt ID is not filed")

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "C1", ephemeral: true, promptID: "P2"})
	a.recordSignInClick("U1", "T1|P2", responseURL)
	a.recordSignInClick("U1", "T1|P1", "https://hooks.slack.test/actions/old")
	anchors = a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.Equal(t, responseURL, anchors[0].responseURL, "a later click on the expired card keeps the current card's handle")

	a.recordSignInAnchor("U1", "T1", signInAnchor{channel: "D1", ts: "p.000"})
	a.recordSignInClick("U1", "T1|P1", responseURL)
	anchors = a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.Empty(t, anchors[0].responseURL, "a DM prompt is rewritten by its ts")

	a.recordSignInAnchor("U1", "", signInAnchor{channel: "C1", ephemeral: true, promptID: "P3"})
	a.recordSignInClick("U1", "|P3", responseURL)
	anchors = a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1)
	require.Equal(t, responseURL, anchors[0].responseURL, "a top-level prompt takes its click")
}

// A channel prompt posts with a fresh prompt ID in its button and on its
// anchor, so a later click can be matched to the card.
func TestPostSignIn_ChannelPromptCarriesItsPromptID(t *testing.T) {
	a, _ := newTestAdapter(t)
	anchor, err := a.postSignInPrompt(t.Context(), "C1", "T1", "U1", "https://example.test/link", false, signInForMessage)
	require.NoError(t, err)
	require.NotEmpty(t, anchor.promptID)
	again, err := a.postSignInPrompt(t.Context(), "C1", "T1", "U1", "https://example.test/link", false, signInForMessage)
	require.NoError(t, err)
	require.NotEqual(t, anchor.promptID, again.promptID, "each card has its own ID")
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
	require.Equal(t, "Waiting for <@U1> and <@U2> to sign in to Giant Swarm"+signInWaitingDMHint, latest())

	a.updateSignInAnchors(t.Context(), "U1") // U1's link completes: the notice, then U1's DM card
	srv.mu.Lock()
	notice := srv.updateTexts[len(srv.updateTexts)-2]
	srv.mu.Unlock()
	require.Equal(t, "Waiting for <@U2> to sign in to Giant Swarm"+signInWaitingDMHint, notice)
	require.Equal(t, signedInNotice, latest(), "U1's DM card confirms the sign-in")

	a.updateSignInAnchors(t.Context(), "U2")
	srv.mu.Lock()
	notice = srv.updateTexts[len(srv.updateTexts)-2]
	srv.mu.Unlock()
	require.Equal(t, "<@U1> and <@U2> signed in to Giant Swarm", notice)

	updates := srv.updates.Load()
	a.updateSignInAnchors(t.Context(), "U2")
	require.Equal(t, updates, srv.updates.Load(), "a notice that does not wait for the user is left alone")

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage) // U1 signed out and writes again
	require.Equal(t, int32(1), srv.posts.Load(), "the thread keeps its one notice")
	require.Equal(t, "Waiting for <@U1> to sign in to Giant Swarm"+signInWaitingDMHint, latest())

	a.postSignIn(t.Context(), "C2", "T2", "U1", false, signInForMessage)
	require.Equal(t, int32(2), srv.posts.Load(), "a different thread gets its own notice")
}

// The card's last line follows what asked for it: a held message runs by
// itself, a button click is clicked again, /login needs nothing more.
func TestSignInPromptBody_Trigger(t *testing.T) {
	text := func(trigger signInTrigger) string {
		return signInPromptBody("C1", "T1", "https://example.test/link", "", false, trigger)[paramText].(string)
	}
	require.True(t, strings.HasSuffix(text(signInForMessage), signInForMessageLine))
	require.True(t, strings.HasSuffix(text(signInForClick), signInForClickLine))
	require.True(t, strings.HasSuffix(text(signInForLogin), "The link is valid for 15 minutes."))

	blocks := signInPromptBody("C1", "T1", "https://example.test/link", "P1", true, signInForLogin)[paramBlocks].([]any)
	require.Len(t, blocks, 4, "superseded note, section, button, session hint")
	button := func(blocks []any) map[string]any {
		return blocks[len(blocks)-2].(map[string]any)[bkElements].([]any)[0].(map[string]any)
	}
	require.Equal(t, "T1|P1", button(blocks)[bkValue], "a channel prompt's click names its thread and the prompt")
	require.Equal(t, "|P1", button(signInPromptBody("C1", "", "https://example.test/link", "P1", false, signInForLogin)[paramBlocks].([]any))[bkValue],
		"a top-level prompt's value still names the prompt")
	_, hasValue := button(signInPromptBody("D1", "T1", "https://example.test/link", "", false, signInForLogin)[paramBlocks].([]any))[bkValue]
	require.False(t, hasValue, "a DM prompt, rewritten by its ts, carries no value")
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

// An ephemeral reaches only a Slack client that is open when it is sent, so a
// channel prompt also puts its card into the person's DM, where it waits; the
// thread notice says so, and links nothing (klaus-gateway#406). The DM card
// names the thread it came from, holds one live card however often the prompt
// is re-issued, and confirms the completed link in place.
func TestPostSignIn_ChannelPromptWaitsInTheDM(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{}

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage)

	require.Equal(t, int32(1), srv.dms.Load(), "the card goes to the person's DM")
	require.Equal(t, int32(1), srv.ephemerals.Load(), "the ephemeral still reaches an open client")
	srv.mu.Lock()
	dm, notice := srv.dmBodies[0], srv.postTexts[0]
	srv.mu.Unlock()
	require.Contains(t, dm, "example.test/link", "the DM card carries the link")
	require.Contains(t, dm, "https://slack.test/archives/C1/pT1", "the DM card links back to the thread")
	require.Equal(t, "Waiting for <@U1> to sign in to Giant Swarm"+signInWaitingDMHint, notice)

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage)
	require.Equal(t, int32(1), srv.dms.Load(), "a re-issued prompt refreshes the DM card instead of adding one")
	srv.mu.Lock()
	refresh := srv.updateBodies[len(srv.updateBodies)-1]
	srv.mu.Unlock()
	require.Contains(t, refresh, `"channel":"D1"`, "the refresh edits the card in the D… conversation Slack named")
	require.Contains(t, refresh, `"ts":"dm.0001"`)
	require.Contains(t, refresh, "example.test/link", "the refreshed card carries the fresh link")

	a.updateSignInAnchors(t.Context(), "U1") // the link completes
	srv.mu.Lock()
	confirm, confirmBody := srv.updateTexts[len(srv.updateTexts)-1], srv.updateBodies[len(srv.updateBodies)-1]
	srv.mu.Unlock()
	require.Equal(t, signedInNotice, confirm, "the DM card confirms the sign-in in place")
	require.Contains(t, confirmBody, `"channel":"D1"`)
	require.NotContains(t, confirmBody, "example.test/link", "the confirmed card drops its button")
}

// A person still waiting who writes again in the thread after the burst window
// gets the prompt again: a fresh ephemeral for the client they have open now,
// and the DM card refreshed to the same fresh link. The old link is still
// valid, so the fresh card does not call it expired.
func TestMaybePostSignIn_LaterMentionReissuesTheChannelPrompt(t *testing.T) {
	a, srv := newTestAdapter(t)
	a.OBO = deadLinkOBO{}

	a.maybePostSignIn(t.Context(), "C1", "T1", "U1", signInForMessage)
	a.maybePostSignIn(t.Context(), "C1", "T1", "U1", signInForMessage)
	require.Equal(t, int32(1), srv.ephemerals.Load(), "a burst prompts once")

	a.signInPromptedMu.Lock()
	entry := a.signInPrompted["U1\x00T1"]
	entry.value.nudgedAt = time.Now().Add(-signInReissueAfter)
	a.signInPrompted["U1\x00T1"] = entry
	a.signInPromptedMu.Unlock()
	updates := srv.updates.Load()

	a.maybePostSignIn(t.Context(), "C1", "T1", "U1", signInForMessage)

	require.Equal(t, int32(2), srv.ephemerals.Load(), "the later mention re-issues the prompt")
	require.Equal(t, int32(1), srv.dms.Load(), "the DM keeps one card")
	require.Equal(t, updates+1, srv.updates.Load(), "the DM card is refreshed; the notice already names the user")
	require.Equal(t, int32(1), srv.posts.Load(), "the thread keeps its one notice")
	srv.mu.Lock()
	reissued := srv.ephemeralTexts[1]
	srv.mu.Unlock()
	require.Contains(t, reissued, "*Sign in to Giant Swarm*")
	require.NotContains(t, reissued, signInLinkSupersededNote, "the earlier link is still alive")
}

// A DM that cannot be sent costs only the DM: the ephemeral still goes out, and
// the thread notice does not point at a card that is not there.
func TestPostSignIn_FailedDMLeavesTheNoticeHintOut(t *testing.T) {
	fake := newFakeSlackServerFailingDMs()
	a, _ := newTestAdapter(t)
	a.APIBase = fake.URL
	a.OBO = deadLinkOBO{}

	a.postSignIn(t.Context(), "C1", "T1", "U1", false, signInForMessage)

	require.Equal(t, []string{"Waiting for <@U1> to sign in to Giant Swarm"}, fake.threadTexts())
	require.Equal(t, 1, fake.ephemeralCount(), "the ephemeral prompt still posts")
	anchors := a.takeSignInAnchors("U1")
	require.Len(t, anchors, 1, "only the ephemeral is anchored")
	require.True(t, anchors[0].ephemeral)
}

// failingDMServer is a Slack API that refuses every post addressed to a user,
// the way a workspace that turned the app's messages tab off does.
type failingDMServer struct {
	*httptest.Server
	mu         sync.Mutex
	threads    []string
	ephemerals int
}

func newFakeSlackServerFailingDMs() *failingDMServer {
	f := &failingDMServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		channel, _ := body["channel"].(string)
		text, _ := body["text"].(string)
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "chat.postMessage") && strings.HasPrefix(channel, "U"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "messages_tab_disabled"})
		case strings.HasSuffix(r.URL.Path, "chat.postMessage"):
			f.threads = append(f.threads, text)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
		case strings.HasSuffix(r.URL.Path, "chat.postEphemeral"):
			f.ephemerals++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	return f
}

func (f *failingDMServer) threadTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.threads...)
}

func (f *failingDMServer) ephemeralCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ephemerals
}
