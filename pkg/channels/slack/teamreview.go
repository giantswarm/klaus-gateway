package slack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// The team review is the prompt kind a manager posts into a team's channel
// through POST /reviews (pkg/reviews): the change spelled out, an Approve
// button and a link for anything else. It has no thread initiator; the
// decision rule is the team's — any Slack user with a linked identity may
// click, and the click calls the review's tool as that person. Membership of
// the named team is the tool's to check (it holds the person's GitHub grant);
// a refusal there leaves the review open for another member. One approval
// closes it: a later click is told who decided.

// Team-review action IDs.
const (
	teamReviewApprove = "team_review_approve" // the Approve button
	teamReviewOpen    = "team_review_open"    // the URL button; the browser opens it, no handling
)

// teamReviewTTL bounds how long an undecided review stays clickable. A click
// past it rewrites the message to say so; the manager may post the ask again.
const teamReviewTTL = 7 * 24 * time.Hour

// Team-review notices.
const (
	teamReviewExpiredNotice = "_This review has expired. Ask for it to be posted again._"
	teamReviewDecidedNotice = "This review was already approved by <@%s>."
	teamReviewPendingNotice = "<@%s>'s approval is being submitted right now."
	teamReviewRefusedNotice = "❌ Your approval was not accepted: %s"
	teamReviewFailedNotice  = "⚠️ I couldn't submit your approval to the manager. Try again in a moment."
)

// ToolCaller calls a muster tool as the person whose bearer token it is
// given. *muster.Client satisfies it.
type ToolCaller interface {
	CallTool(ctx context.Context, bearer, tool string, args map[string]any) (muster.Result, error)
}

// teamReview is one posted review and its decision state.
type teamReview struct {
	channels.TeamReview
	id       string
	ts       string
	postedAt time.Time
	// decidedBy is the Slack user whose approval is in flight or done; "" while
	// the review is open.
	decidedBy string
	done      bool
}

// teamReviewValue is the Approve button's value: the review id.
type teamReviewValue struct {
	Review string `json:"r"`
}

func encodeTeamReviewValue(id string) string {
	b, _ := json.Marshal(teamReviewValue{Review: id})
	return string(b)
}

func decodeTeamReviewValue(raw string) (string, bool) {
	var v teamReviewValue
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v.Review == "" {
		return "", false
	}
	return v.Review, true
}

func newTeamReviewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// PostTeamReview posts the ask into its channel and records it so a click can
// be resolved. It implements channels.TeamReviewPoster.
func (a *Adapter) PostTeamReview(ctx context.Context, review channels.TeamReview) (channels.PostReceipt, error) {
	if a.Tools == nil || a.OBO == nil {
		return channels.PostReceipt{}, errors.New("slack: team reviews need a tool caller and account linking")
	}
	id, err := newTeamReviewID()
	if err != nil {
		return channels.PostReceipt{}, fmt.Errorf("slack: team review id: %w", err)
	}
	ts, err := a.apiClient().postJSON(ctx, methodChatPostMessage, map[string]any{
		paramChannel: review.Channel,
		paramText:    teamMessageFallback(review.Team, review.Text),
		paramBlocks:  teamReviewBlocks(review, id),
	})
	if err != nil {
		return channels.PostReceipt{}, err
	}
	a.storeTeamReview(&teamReview{TeamReview: review, id: id, ts: ts, postedAt: time.Now()})
	return channels.PostReceipt{ID: id, Channel: review.Channel, TS: ts}, nil
}

// PostTeamNotice posts a message that asks for nothing: no buttons, the link
// (when given) inline. It implements channels.TeamReviewPoster.
func (a *Adapter) PostTeamNotice(ctx context.Context, notice channels.TeamNotice) (channels.PostReceipt, error) {
	ts, err := a.apiClient().postJSON(ctx, methodChatPostMessage, map[string]any{
		paramChannel: notice.Channel,
		paramText:    teamMessageFallback(notice.Team, notice.Text),
		paramBlocks:  teamNoticeBlocks(notice),
	})
	if err != nil {
		return channels.PostReceipt{}, err
	}
	return channels.PostReceipt{Channel: notice.Channel, TS: ts}, nil
}

// teamMessageFallback is the notification text of a review or notice.
func teamMessageFallback(team, text string) string {
	return truncateRunes(fmt.Sprintf("%s: %s", team, text), slackSectionTextMax)
}

// teamHeading is the line naming the team the message is for. The manager's
// text is trusted mrkdwn (an allow-listed service wrote it); the team name is
// escaped since it is quoted, not authored.
func teamHeading(team, text string) string {
	return truncateRunes(fmt.Sprintf("*Review for %s*\n%s", escapeMrkdwn(team), text), slackSectionTextMax)
}

func teamReviewBlocks(review channels.TeamReview, id string) []any {
	elements := []any{
		map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: "✅ Approve"},
			bkStyle:    bkPrimary,
			bkActionID: teamReviewApprove,
			bkValue:    encodeTeamReviewValue(id),
		},
	}
	if review.Link != "" {
		elements = append(elements, map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: "Open PR"},
			bkActionID: teamReviewOpen,
			bkURL:      review.Link,
			bkValue:    encodeTeamReviewValue(id),
		})
	}
	return []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: teamHeading(review.Team, review.Text)}},
		map[string]any{bkType: bkActions, bkElements: elements},
	}
}

func teamNoticeBlocks(notice channels.TeamNotice) []any {
	text := truncateRunes(fmt.Sprintf("*For %s*\n%s", escapeMrkdwn(notice.Team), notice.Text), slackSectionTextMax)
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: text}},
	}
	if notice.Link != "" {
		blocks = append(blocks, map[string]any{
			bkType:     bkContext,
			bkElements: []any{map[string]any{bkType: bkMrkdwn, bkText: fmt.Sprintf("<%s|Open PR>", notice.Link)}},
		})
	}
	return blocks
}

// handleTeamReviewDecision resolves an Approve click: the clicker must have a
// linked identity (an unlinked one is asked to sign in, the review stays
// open), the review must still be open (a second click is told who decided),
// and the tool is called as the clicker. The tool's refusal — the manager
// finding the person outside the team, or anything else it will not do — is
// shown to the clicker alone and reopens the review; its success is written
// into the message with the decider.
func (a *Adapter) handleTeamReviewDecision(ctx context.Context, slackChannel, messageTS, clicker, value string) {
	client := a.apiClient()
	id, ok := decodeTeamReviewValue(value)
	if !ok {
		return
	}
	rv := a.lookupTeamReview(id)
	if rv == nil {
		if err := client.chatUpdateBlocks(ctx, slackChannel, messageTS, teamReviewExpiredNotice); err != nil {
			a.Logger.Warn("slack: team review expired rewrite failed", "review", id, "error", err)
		}
		return
	}
	if a.Tools == nil {
		a.Logger.Error("slack: team review click without a tool caller", "review", id)
		return
	}

	// The person's own token is the identity the tool runs under; without a
	// link there is nobody to act as, so the click is turned into a sign-in.
	token, ok, signIn := a.humanToken(ctx, rv.Channel, rv.ts, clicker)
	if signIn {
		a.postSignIn(ctx, rv.Channel, rv.ts, clicker, false)
	}
	if !ok {
		return
	}

	holder, claimed := a.claimTeamReview(id, clicker)
	if !claimed {
		notice := fmt.Sprintf(teamReviewDecidedNotice, holder)
		if !a.teamReviewDone(id) {
			notice = fmt.Sprintf(teamReviewPendingNotice, holder)
		}
		a.tellClicker(ctx, rv, clicker, notice)
		return
	}

	res, err := a.Tools.CallTool(ctx, token, rv.Approve.Tool, rv.Approve.Arguments)
	switch {
	case err != nil:
		a.releaseTeamReview(id)
		a.Logger.Warn("slack: team review tool call failed", "review", id, "team", rv.Team, "tool", rv.Approve.Tool, "user", clicker, "error", err)
		a.tellClicker(ctx, rv, clicker, teamReviewFailedNotice)
		return
	case res.IsError:
		a.releaseTeamReview(id)
		a.Logger.Info("slack: team review approval refused by the tool", "record", "team_review_refused",
			"review", id, "team", rv.Team, "tool", rv.Approve.Tool, "slack_user", clicker, "reason", res.Text)
		reason := "the manager refused it"
		if res.Text != "" {
			reason = truncateRunes(escapeMrkdwn(res.Text), 500)
		}
		a.tellClicker(ctx, rv, clicker, fmt.Sprintf(teamReviewRefusedNotice, reason))
		return
	}

	a.finishTeamReview(id)
	a.Logger.Info("slack: team review approved", "record", "team_review_approved",
		"review", id, "team", rv.Team, "tool", rv.Approve.Tool, "slack_user", clicker, "subject", a.linkedSubject(clicker))
	if err := client.chatUpdateBlocks(ctx, rv.Channel, rv.ts, teamReviewOutcome(rv, clicker, res.Text)); err != nil {
		a.Logger.Warn("slack: team review outcome rewrite failed", "review", id, "error", err)
	}
}

// teamReviewOutcome is the text the review message is rewritten to once it is
// approved: who decided, for which team, the ask, and what the tool said.
func teamReviewOutcome(rv *teamReview, decider, result string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ *Approved* by <@%s> for %s.\n%s", decider, escapeMrkdwn(rv.Team), rv.Text)
	if result != "" {
		fmt.Fprintf(&b, "\n_%s_", truncateRunes(escapeMrkdwn(result), 500))
	}
	return truncateRunes(b.String(), slackSectionTextMax)
}

// tellClicker answers a click that changed nothing, visibly only to the
// clicker, in the review's thread.
func (a *Adapter) tellClicker(ctx context.Context, rv *teamReview, clicker, text string) {
	if err := a.apiClient().postEphemeralText(ctx, rv.Channel, clicker, rv.ts, text); err != nil {
		a.Logger.Warn("slack: team review notice failed", "review", rv.id, "user", clicker, "error", err)
	}
}

// linkedSubject is the muster subject of a linked Slack user, for the record;
// empty when the token source does not expose identities.
func (a *Adapter) linkedSubject(slackUser string) string {
	if ident, ok := a.OBO.(linkedIdentitySource); ok {
		sub, _, _ := ident.LinkedIdentity(slackUser)
		return sub
	}
	return ""
}

// --- review records: in-memory, guarded by teamReviewsMu ---

func (a *Adapter) storeTeamReview(rv *teamReview) {
	a.teamReviewsMu.Lock()
	defer a.teamReviewsMu.Unlock()
	if a.teamReviews == nil {
		a.teamReviews = map[string]*teamReview{}
	}
	cutoff := time.Now().Add(-teamReviewTTL)
	for id, old := range a.teamReviews {
		if old.postedAt.Before(cutoff) {
			delete(a.teamReviews, id)
		}
	}
	a.teamReviews[rv.id] = rv
}

// lookupTeamReview returns a copy of the record, or nil when it is unknown or
// past its TTL.
func (a *Adapter) lookupTeamReview(id string) *teamReview {
	a.teamReviewsMu.Lock()
	defer a.teamReviewsMu.Unlock()
	rv, ok := a.teamReviews[id]
	if !ok || time.Since(rv.postedAt) > teamReviewTTL {
		return nil
	}
	cp := *rv
	return &cp
}

// claimTeamReview marks the review as being decided by user. It reports the
// current holder and false when another approval is in flight or done.
func (a *Adapter) claimTeamReview(id, user string) (holder string, claimed bool) {
	a.teamReviewsMu.Lock()
	defer a.teamReviewsMu.Unlock()
	rv, ok := a.teamReviews[id]
	if !ok {
		return "", false
	}
	if rv.decidedBy != "" {
		return rv.decidedBy, false
	}
	rv.decidedBy = user
	return "", true
}

// releaseTeamReview reopens a review whose approval did not go through.
func (a *Adapter) releaseTeamReview(id string) {
	a.teamReviewsMu.Lock()
	defer a.teamReviewsMu.Unlock()
	if rv, ok := a.teamReviews[id]; ok && !rv.done {
		rv.decidedBy = ""
	}
}

// finishTeamReview records the approval as done; the record stays until its
// TTL so a late click is told who decided.
func (a *Adapter) finishTeamReview(id string) {
	a.teamReviewsMu.Lock()
	defer a.teamReviewsMu.Unlock()
	if rv, ok := a.teamReviews[id]; ok {
		rv.done = true
	}
}

func (a *Adapter) teamReviewDone(id string) bool {
	a.teamReviewsMu.Lock()
	defer a.teamReviewsMu.Unlock()
	rv, ok := a.teamReviews[id]
	return ok && rv.done
}
