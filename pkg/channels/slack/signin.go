package slack

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// signInAnchor is the message coordinates of a posted sign-in prompt, kept so
// a completed link can rewrite the prompt in place (chat.update). threadID is
// filled by takeSignInAnchors from the map key so the rewrite can decide, per
// thread, whether that thread's replay reaches the agent.
//
// The entry plays two roles with different lifetimes: as a rewrite anchor it
// must stay addressable for pendingTTL (a link can complete long after the
// prompt posted), while as the nudge throttle it expires with the link URL's
// state lifetime (nudgedAt vs signInNudgeTTL), so a user facing a dead button
// gets a fresh prompt instead of silence.
type signInAnchor struct {
	channel  string
	ts       string
	threadID string
	nudgedAt time.Time // when the prompt for this (user, thread) last posted
}

// postSignIn posts the "Sign in" prompt for the account-linking flow and
// records its message coordinates so the completed link rewrites it in place.
// It is driven by the explicit /login command and by an unlinked user's first
// turn (which is aborted, not run as the SA). A failure to post is logged and
// swallowed.
func (a *Adapter) postSignIn(ctx context.Context, slackChannel, threadID, slackUser string) {
	url := a.OBO.LinkURL(slackUser)
	if url == "" {
		a.Logger.Warn("slack: empty sign-in link URL, skipping prompt", "user", slackUser)
		a.clearSignInReservation(slackUser, threadID)
		return
	}
	ts, err := a.apiClient().postSignInPrompt(ctx, slackChannel, threadID, slackUser, url)
	if err != nil {
		a.Logger.Warn("slack: post sign-in prompt failed", "user", slackUser, "error", err)
		a.clearSignInReservation(slackUser, threadID)
		return
	}
	a.recordSignInAnchor(slackUser, threadID, signInAnchor{channel: slackChannel, ts: ts})
	// The post ran outside any lock, so a link callback may have drained the
	// anchors while the prompt was in flight; the just-recorded anchor would
	// then keep a live sign-in button for an already-linked user and suppress
	// re-prompts for the full window. Re-check and converge.
	if _, err := a.OBO.TokenFor(ctx, slackUser); err == nil {
		a.updateSignInAnchors(ctx, slackUser, nil)
	}
}

// clearSignInReservation removes the (user, thread) throttle entry when no
// prompt message exists for it, so a failed post retries on the next parked
// message instead of suppressing the nudge for the full window. An entry with
// a posted anchor is left alone.
func (a *Adapter) clearSignInReservation(slackUser, threadID string) {
	key := slackUser + "\x00" + threadID
	a.signInPromptedMu.Lock()
	defer a.signInPromptedMu.Unlock()
	if entry, ok := a.signInPrompted[key]; ok && entry.value.ts == "" {
		delete(a.signInPrompted, key)
	}
}

// recordSignInAnchor stores the posted prompt's coordinates under the (user,
// thread) key. It also arms the nudge throttle (nudgedAt), so an explicit
// /login prompt suppresses a redundant parked-message nudge in the same
// thread while its link is alive.
func (a *Adapter) recordSignInAnchor(slackUser, threadID string, anchor signInAnchor) {
	now := time.Now()
	key := slackUser + "\x00" + threadID
	anchor.nudgedAt = now
	a.signInPromptedMu.Lock()
	defer a.signInPromptedMu.Unlock()
	if a.signInPrompted == nil {
		a.signInPrompted = make(map[string]ttlEntry[signInAnchor])
	}
	sweepExpired(a.signInPrompted, now)
	a.signInPrompted[key] = ttlEntry[signInAnchor]{value: anchor, expires: now.Add(pendingTTL)}
}

// takeSignInAnchors returns and clears the user's sign-in prompt entries,
// keeping only anchors that are still addressable (posted successfully and
// unexpired). Draining doubles as the throttle reset, so becoming unlinked
// again (e.g. /logout) prompts anew.
func (a *Adapter) takeSignInAnchors(slackUser string) []signInAnchor {
	prefix := slackUser + "\x00"
	now := time.Now()
	a.signInPromptedMu.Lock()
	defer a.signInPromptedMu.Unlock()
	var anchors []signInAnchor
	for key, entry := range a.signInPrompted {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		delete(a.signInPrompted, key)
		if entry.value.ts != "" && now.Before(entry.expires) {
			anchor := entry.value
			anchor.threadID = strings.TrimPrefix(key, prefix)
			anchors = append(anchors, anchor)
		}
	}
	return anchors
}

// updateSignInAnchors rewrites the user's sign-in prompt messages in place once
// the account link completes, folding the confirmation into the message the
// user is already looking at (the URL button is dropped by the rewrite). The
// identity the user signed in as is confirmed on the private browser success
// page, not here, so the in-thread rewrite carries no email. An anchor whose
// thread is in replayingThreads (a parked message about to reach the agent)
// gains the handoff notice; the others keep the plain confirmation.
func (a *Adapter) updateSignInAnchors(ctx context.Context, slackUser string, replayingThreads map[string]bool) {
	anchors := a.takeSignInAnchors(slackUser)
	if len(anchors) == 0 {
		return
	}
	const base = "✅ Signed in. I can act on your behalf now."
	var handoff string
	if len(replayingThreads) > 0 {
		handoff = fmt.Sprintf(" Bringing in **%s** to help.", a.agentDisplayName(ctx, a.DefaultAgent))
	}
	client := a.apiClient()
	for _, anchor := range anchors {
		text := base
		if replayingThreads[anchor.threadID] {
			text += handoff
		}
		if err := client.chatUpdateMarkdown(ctx, anchor.channel, anchor.ts, text); err != nil {
			a.Logger.Warn("slack: update sign-in prompt after link failed", "user", slackUser, "channel", anchor.channel, "ts", anchor.ts, "error", err)
			// The anchor was drained before the update; without it the thread
			// would keep a live sign-in button for a linked user forever.
			// Re-recording lets the next convergence pass retry the rewrite.
			a.recordSignInAnchor(slackUser, anchor.threadID, signInAnchor{channel: anchor.channel, ts: anchor.ts})
		}
	}
}

// shouldPostSignInNudge reports whether a fresh sign-in prompt may post now
// for a (user, thread) whose throttle entry is entry (exists=false when none
// is recorded). The throttle re-arms on signInNudgeTTL, the link URL's state
// lifetime, not on the entry's own pendingTTL expiry: the entry outlives the
// link by design (it doubles as the rewrite anchor), and suppressing the nudge
// for that long would leave the user parked behind a dead button.
func shouldPostSignInNudge(entry ttlEntry[signInAnchor], exists bool, now time.Time) bool {
	if !exists || now.After(entry.expires) {
		return true
	}
	return now.Sub(entry.value.nudgedAt) >= signInNudgeTTL
}

// maybePostSignIn posts the sign-in prompt unless one with a live link was
// already posted for this (user, thread) (see shouldPostSignInNudge), so a
// burst of parked messages nudges once instead of once per message. The
// explicit /login command bypasses it (postSignIn directly); a completed link
// drains the window (takeSignInAnchors) so a /logout re-prompts. The entry is
// reserved (with no anchor yet) before posting so concurrent parks nudge once;
// postSignIn overwrites it with the posted message's coordinates. When a
// re-nudge replaces a prompt whose link expired, the old prompt is rewritten
// (best-effort) so its dead button cannot be mistaken for the live one.
func (a *Adapter) maybePostSignIn(ctx context.Context, slackChannel, threadID, slackUser string) {
	now := time.Now()
	key := slackUser + "\x00" + threadID
	a.signInPromptedMu.Lock()
	entry, prompted := a.signInPrompted[key]
	if !shouldPostSignInNudge(entry, prompted, now) {
		a.signInPromptedMu.Unlock()
		return
	}
	var expired signInAnchor
	if prompted && now.Before(entry.expires) {
		expired = entry.value
	}
	if a.signInPrompted == nil {
		a.signInPrompted = make(map[string]ttlEntry[signInAnchor])
	}
	sweepExpired(a.signInPrompted, now)
	a.signInPrompted[key] = ttlEntry[signInAnchor]{value: signInAnchor{nudgedAt: now}, expires: now.Add(pendingTTL)}
	a.signInPromptedMu.Unlock()
	if expired.ts != "" {
		if err := a.apiClient().chatUpdateMarkdown(ctx, expired.channel, expired.ts, signInLinkExpiredNote); err != nil {
			a.Logger.Warn("slack: rewrite expired sign-in prompt failed", "user", slackUser, "thread", threadID, "error", err)
		}
	}
	a.postSignIn(ctx, slackChannel, threadID, slackUser)
}
