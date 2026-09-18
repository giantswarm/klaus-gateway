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
//
// A review is a channel message, not a thread: what the gateway has to say to
// one clicker (a refusal, a sign-in) is a channel-level ephemeral, which Slack
// shows right where they clicked, and what the team should see (who is
// connecting, whose approval was refused) is a status line on the message
// itself, replaced on every attempt and gone once the review is approved.

// Team-review action IDs.
const (
	teamReviewApprove = "team_review_approve" // the Approve button
	teamReviewOpen    = "team_review_open"    // the URL button; the browser opens it, no handling
)

// teamReviewTTL bounds how long an undecided review stays clickable. A click
// past it rewrites the message to say so; the manager may post the ask again.
const teamReviewTTL = 7 * 24 * time.Hour

// Team-review notices to one clicker.
const (
	teamReviewExpiredNotice = "_This review has expired. Ask for it to be posted again._"
	teamReviewDecidedNotice = "This review was already approved by <@%s>."
	teamReviewPendingNotice = "<@%s>'s approval is being submitted right now."
	teamReviewRefusedNotice = "❌ Your approval was not accepted: %s"
	teamReviewFailedNotice  = "⚠️ I couldn't submit your approval to the manager. Try again in a moment."
	// teamReviewConnectNotice heads the Connect prompt when the sign-in lands
	// back on the gateway and the approval is resubmitted by itself;
	// teamReviewConnectManualNotice when it does not.
	teamReviewConnectNotice       = "To approve as yourself, connect *%s* once. Your approval is submitted as soon as you're back."
	teamReviewConnectManualNotice = "To approve as yourself, connect *%s* once, then click *Approve* again."
	teamReviewStillChallenged     = "⚠️ I still can't reach %s as you after the sign-in. Click *Approve* again in a moment."
)

// Team-review status lines, shown to the whole team under the buttons. Each
// replaces the previous one; the approval outcome replaces them all.
const (
	teamReviewStatusConnecting = "🔗 <@%s> is connecting *%s* to approve as themselves."
	teamReviewStatusRefused    = "❌ <@%s>'s approval was not accepted: %s"
	teamReviewStatusFailed     = "⚠️ <@%s>'s approval could not be submitted to the manager; they can try again."
)

// teamReviewReasonMax bounds a tool's refusal text in the status line; the
// clicker's own notice carries more of it.
const (
	teamReviewReasonMax       = 300
	teamReviewNoticeReasonMax = 500
)

// teamReviewUnknownServer names the backend in a Connect prompt when muster's
// challenge does not.
const teamReviewUnknownServer = "the manager"

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
	// status is the line the team sees under the buttons: the latest attempt
	// that did not decide the review. "" shows none.
	status string
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
		paramBlocks:  teamReviewBlocks(review, id, ""),
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

// teamReviewBlocks renders an open review: the ask, the buttons and, when
// status is set, the status line the team sees under them.
func teamReviewBlocks(review channels.TeamReview, id, status string) []any {
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
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: teamHeading(review.Team, review.Text)}},
		map[string]any{bkType: bkActions, bkElements: elements},
	}
	if status != "" {
		blocks = append(blocks, contextBlock(status))
	}
	return blocks
}

func teamNoticeBlocks(notice channels.TeamNotice) []any {
	text := truncateRunes(fmt.Sprintf("*For %s*\n%s", escapeMrkdwn(notice.Team), notice.Text), slackSectionTextMax)
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: text}},
	}
	if notice.Link != "" {
		blocks = append(blocks, contextBlock(openLink(notice.Link)))
	}
	return blocks
}

// openLink is the link of a review or notice as inline mrkdwn.
func openLink(link string) string {
	return fmt.Sprintf("<%s|Open PR>", link)
}

// handleTeamReviewDecision resolves an Approve click against the review
// record: an unknown or expired review rewrites the dead button, a known one
// is decided as the clicker.
func (a *Adapter) handleTeamReviewDecision(ctx context.Context, slackChannel, messageTS, clicker, value string) {
	id, ok := decodeTeamReviewValue(value)
	if !ok {
		return
	}
	rv := a.lookupTeamReview(id)
	if rv == nil {
		if err := a.apiClient().chatUpdateBlocks(ctx, slackChannel, messageTS, teamReviewExpiredNotice); err != nil {
			a.Logger.Warn("slack: team review expired rewrite failed", "review", id, "error", err)
		}
		return
	}
	if a.Tools == nil {
		a.Logger.Error("slack: team review click without a tool caller", "review", id)
		return
	}
	a.decideTeamReview(ctx, rv, clicker, false)
}

// decideTeamReview submits the review's approval as clicker: the clicker must
// have a linked identity (an unlinked one is asked to sign in, the review
// stays open), the review must still be open (a second click is told who
// decided), and the tool is called as the clicker.
//
// What the tool answers decides the rest. A sign-in challenge — muster does
// not hold the person's grant for the tool's backend yet — turns into a
// Connect prompt for the clicker; the sign-in lands back on the gateway and
// the approval is submitted again (resumed=true), so the person connects once
// and never clicks twice. A refusal — the manager finding the person outside
// the team, the author of their own change, or anything else it will not do
// — is shown to the clicker and, in one line, to the team, and reopens the
// review; so is a manager the gateway could not reach. A success is written
// into the message with the decider.
func (a *Adapter) decideTeamReview(ctx context.Context, rv *teamReview, clicker string, resumed bool) {
	// The person's own token is the identity the tool runs under; without a
	// link there is nobody to act as, so the click is turned into a sign-in.
	token, ok, signIn := a.humanToken(ctx, rv.Channel, "", clicker)
	if signIn {
		a.postSignIn(ctx, rv.Channel, "", clicker, false)
	}
	if !ok {
		return
	}

	holder, claimed := a.claimTeamReview(rv.id, clicker)
	if !claimed {
		notice := fmt.Sprintf(teamReviewDecidedNotice, holder)
		if !a.teamReviewDone(rv.id) {
			notice = fmt.Sprintf(teamReviewPendingNotice, holder)
		}
		a.tellClicker(ctx, rv, clicker, notice)
		return
	}

	res, err := a.Tools.CallTool(ctx, token, rv.Approve.Tool, rv.Approve.Arguments)
	if err != nil {
		a.releaseTeamReview(rv.id)
		a.Logger.Warn("slack: team review tool call failed", "review", rv.id, "team", rv.Team, "tool", rv.Approve.Tool, "user", clicker, "error", err)
		a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusFailed, clicker))
		a.tellClicker(ctx, rv, clicker, teamReviewFailedNotice)
		return
	}
	if server, loginURL, challenged := authChallengeOf(res); challenged {
		a.releaseTeamReview(rv.id)
		if resumed {
			// The sign-in landed and the backend still challenges: do not loop
			// the person through the consent flow again on their behalf.
			a.Logger.Warn("slack: team review still challenged after the connector sign-in", "review", rv.id, "team", rv.Team, "tool", rv.Approve.Tool, "user", clicker, "server", server)
			a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusFailed, clicker))
			a.tellClicker(ctx, rv, clicker, fmt.Sprintf(teamReviewStillChallenged, escapeMrkdwn(server)))
			return
		}
		a.promptTeamReviewConnect(ctx, rv, clicker, server, loginURL)
		return
	}
	if res.IsError {
		a.releaseTeamReview(rv.id)
		a.Logger.Info("slack: team review approval refused by the tool", "record", "team_review_refused",
			"review", rv.id, "team", rv.Team, "tool", rv.Approve.Tool, "slack_user", clicker, "reason", res.Text)
		reason := "the manager refused it"
		if res.Text != "" {
			reason = escapeMrkdwn(res.Text)
		}
		a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusRefused, clicker, truncateRunes(reason, teamReviewReasonMax)))
		a.tellClicker(ctx, rv, clicker, fmt.Sprintf(teamReviewRefusedNotice, truncateRunes(reason, teamReviewNoticeReasonMax)))
		return
	}

	a.finishTeamReview(rv.id)
	a.Logger.Info("slack: team review approved", "record", "team_review_approved",
		"review", rv.id, "team", rv.Team, "tool", rv.Approve.Tool, "slack_user", clicker, "subject", a.linkedSubject(clicker), "resumed", resumed)
	if err := a.apiClient().chatUpdate(ctx, rv.Channel, rv.ts, teamReviewOutcome(rv, clicker, res.Text), teamReviewOutcomeBlocks(rv, clicker, res.Text)); err != nil {
		a.Logger.Warn("slack: team review outcome rewrite failed", "review", rv.id, "error", err)
	}
}

// authChallengeOf reads muster's sign-in challenge out of a tool result: the
// backend the person has to connect and the login link. It is the answer
// call_tool gives for a tool whose server holds no grant for the person yet —
// an error result whose text names the server and carries the link.
func authChallengeOf(res muster.Result) (server, loginURL string, ok bool) {
	if !res.IsError {
		return "", "", false
	}
	server, loginURL = parseAuthChallenge(res.Text)
	if loginURL == "" {
		return "", "", false
	}
	challenged := server != "" || strings.Contains(res.Text, "auth_required") || strings.Contains(res.Text, "Authentication Required")
	if !challenged {
		return "", "", false
	}
	if server == "" {
		server = teamReviewUnknownServer
	}
	return server, loginURL, true
}

// promptTeamReviewConnect answers a sign-in challenge: the review is reopened
// for the team, the team sees who is connecting, and the clicker gets a
// Connect button. With a public base URL the login link is decorated with the
// gateway's landing and the completion state remembers the review, so the
// landing submits the approval again as the person; without one the prompt
// asks them to click Approve again afterwards.
func (a *Adapter) promptTeamReviewConnect(ctx context.Context, rv *teamReview, clicker, server, loginURL string) {
	a.Logger.Info("slack: team review needs the person to connect the backend", "record", "team_review_connect",
		"review", rv.id, "team", rv.Team, "tool", rv.Approve.Tool, "slack_user", clicker, "server", server)
	a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusConnecting, clicker, escapeMrkdwn(server)))

	promptURL, connectValue := loginURL, server
	text := fmt.Sprintf(teamReviewConnectManualNotice, escapeMrkdwn(server))
	if base := a.PublicBaseURL; base != "" {
		stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: clicker, server: server, channel: rv.Channel, review: rv.id})
		if decorated, err := decorateConnectorLoginURL(loginURL, base, stateID); err != nil {
			a.Logger.Warn("slack: team review login URL decoration failed, posting plain link", "review", rv.id, "server", server, "error", err)
		} else {
			promptURL, connectValue = decorated, stateID
			text = fmt.Sprintf(teamReviewConnectNotice, escapeMrkdwn(server))
		}
	}
	if err := a.apiClient().postConnectPrompt(ctx, rv.Channel, "", clicker, text, server, promptURL, connectValue, false); err != nil {
		a.Logger.Warn("slack: team review connect prompt failed", "review", rv.id, "user", clicker, "error", err)
	}
}

// resumeTeamReviewApproval is the landing's continuation of a Connect prompt
// posted for a review: the person signed in to the backend, so the approval
// they clicked for is submitted again as them. A review gone meanwhile
// (restart, TTL) is told to the person alone.
func (a *Adapter) resumeTeamReviewApproval(ctx context.Context, entry connectorCompletion) {
	rv := a.lookupTeamReview(entry.review)
	if rv == nil {
		if err := a.apiClient().postEphemeralText(ctx, entry.channel, entry.slackUser, "", teamReviewExpiredNotice); err != nil {
			a.Logger.Warn("slack: team review expired notice failed", "review", entry.review, "user", entry.slackUser, "error", err)
		}
		return
	}
	if a.Tools == nil {
		a.Logger.Error("slack: team review resume without a tool caller", "review", entry.review)
		return
	}
	a.decideTeamReview(ctx, rv, entry.slackUser, true)
}

// teamReviewOutcome is the text the review message is rewritten to once it is
// approved: who decided, for which team, the ask, and what the tool said.
func teamReviewOutcome(rv *teamReview, decider, result string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ *Approved* by <@%s> for %s.\n%s", decider, escapeMrkdwn(rv.Team), rv.Text)
	if result != "" {
		fmt.Fprintf(&b, "\n_%s_", truncateRunes(escapeMrkdwn(result), teamReviewNoticeReasonMax))
	}
	return truncateRunes(b.String(), slackSectionTextMax)
}

// teamReviewOutcomeBlocks renders the approved review: the outcome, and the
// link kept as small print so the pull request stays one click away.
func teamReviewOutcomeBlocks(rv *teamReview, decider, result string) []any {
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: teamReviewOutcome(rv, decider, result)}},
	}
	if rv.Link != "" {
		blocks = append(blocks, contextBlock(openLink(rv.Link)))
	}
	return blocks
}

// showTeamReviewStatus records status as the review's current status line and
// rewrites the message with it, buttons kept.
func (a *Adapter) showTeamReviewStatus(ctx context.Context, rv *teamReview, status string) {
	if !a.setTeamReviewStatus(rv.id, status) {
		return
	}
	blocks := teamReviewBlocks(rv.TeamReview, rv.id, status)
	if err := a.apiClient().chatUpdate(ctx, rv.Channel, rv.ts, teamMessageFallback(rv.Team, rv.Text), blocks); err != nil {
		a.Logger.Warn("slack: team review status rewrite failed", "review", rv.id, "error", err)
	}
}

// tellClicker answers a click that changed nothing, visibly only to the
// clicker, in the channel where they clicked.
func (a *Adapter) tellClicker(ctx context.Context, rv *teamReview, clicker, text string) {
	if err := a.apiClient().postEphemeralText(ctx, rv.Channel, clicker, "", text); err != nil {
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
		rv.status = ""
	}
}

func (a *Adapter) teamReviewDone(id string) bool {
	a.teamReviewsMu.Lock()
	defer a.teamReviewsMu.Unlock()
	rv, ok := a.teamReviews[id]
	return ok && rv.done
}

// setTeamReviewStatus records the status line of an open review. It reports
// false for a review that is unknown or decided already, whose message must
// not be rewritten back to the buttons.
func (a *Adapter) setTeamReviewStatus(id, status string) bool {
	a.teamReviewsMu.Lock()
	defer a.teamReviewsMu.Unlock()
	rv, ok := a.teamReviews[id]
	if !ok || rv.done {
		return false
	}
	rv.status = status
	return true
}
