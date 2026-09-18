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
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
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
// A review is a channel message, not a thread. What happened to the latest
// attempt that did not decide it — who is connecting, whose approval the
// manager refused and why, whose could not be submitted — is a status line
// on the message itself, replaced on every attempt and gone once the review
// is approved: the clicker and the team read it in the same place, once. A
// channel-level ephemeral, which Slack shows right where they clicked, is
// reserved for what is the clicker's alone: a sign-in or Connect button, or a
// click on a review somebody else decided.
//
// The record — the ask, where its message is, the decision state and the
// status line (store.Review) — lives in the gateway's store (Adapter.Reviews)
// for the review's TTL, so on a store that outlives the process a review
// posted before a restart is decided by a click after it. Every transition
// (claim, release, finish, status) is one atomic update of the record, and
// the claim is what makes one approval close the review across replicas.

// Team-review action IDs.
const (
	teamReviewApprove = "team_review_approve" // the Approve button
	teamReviewOpen    = "team_review_open"    // the URL button; the browser opens it, no handling
)

// teamReviewTTL bounds how long an undecided review stays clickable. A click
// past it rewrites the message to say so; the manager may post the ask again.
const teamReviewTTL = 7 * 24 * time.Hour

// teamReviewClaimLease bounds how long a claim without an outcome holds the
// review. A click's tool call is bounded by the muster client's timeout (a
// minute), so a claim older than this was left behind by a process that died
// mid-call — a restart during the call — and the next click may take the
// review over instead of being told for seven days that an approval is in
// flight.
const teamReviewClaimLease = 5 * time.Minute

// Team-review notices to one clicker.
const (
	teamReviewExpiredNotice = "_This review has expired. Ask for it to be posted again._"
	teamReviewDecidedNotice = "This review was already approved by <@%s>."
	teamReviewPendingNotice = "<@%s>'s approval is being submitted right now."
	// teamReviewUnavailableNotice answers a click the gateway could not
	// resolve because its store did not answer; the message keeps its buttons.
	teamReviewUnavailableNotice = "_The review could not be looked up right now. Click *Approve* again in a moment._"
	// teamReviewUnrecordedNotice replaces a review the gateway posted but
	// could not record: its button would never resolve.
	teamReviewUnrecordedNotice = "_This review could not be recorded and cannot be approved here. Ask for it to be posted again._"
	// teamReviewConnectNotice heads the Connect prompt when the sign-in lands
	// back on the gateway and the approval is resubmitted by itself;
	// teamReviewConnectManualNotice when it does not.
	teamReviewConnectNotice       = "To approve as yourself, connect *%s* once. Your approval is submitted as soon as you're back."
	teamReviewConnectManualNotice = "To approve as yourself, connect *%s* once, then click *Approve* again."
)

// Team-review status lines, under the buttons, read by the clicker and the
// team alike. Each replaces the previous one; the approval outcome replaces
// them all.
const (
	teamReviewStatusConnecting      = "🔗 <@%s> is connecting *%s* to approve as themselves."
	teamReviewStatusRefused         = "❌ <@%s>'s approval was not accepted: %s"
	teamReviewStatusFailed          = "⚠️ <@%s>'s approval could not be submitted to the manager; another click may do."
	teamReviewStatusStillChallenged = "⚠️ <@%s> connected *%s*, but the manager still asks them to sign in; another click may do."
)

// teamReviewReasonMax bounds a tool's text — its refusal in the status line,
// its answer in the outcome.
const teamReviewReasonMax = 500

// teamReviewUnknownServer names the backend in a Connect prompt when muster's
// challenge does not.
const teamReviewUnknownServer = "the manager"

// ToolCaller calls a muster tool as the person whose bearer token it is
// given. *muster.Client satisfies it.
type ToolCaller interface {
	CallTool(ctx context.Context, bearer, tool string, args map[string]any) (muster.Result, error)
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
	rv := newTeamReviewRecord(review, id)
	ts, err := a.apiClient().postJSON(ctx, methodChatPostMessage, map[string]any{
		paramChannel: rv.Channel,
		paramText:    teamMessageFallback(rv.Team, rv.Text),
		paramBlocks:  teamReviewBlocks(rv),
	})
	if err != nil {
		return channels.PostReceipt{}, err
	}
	rv.TS = ts
	if err := a.reviews().PutReview(ctx, rv); err != nil {
		// The message is up but nothing will resolve its button: say so in
		// its place rather than leave a button that reads "expired" on the
		// first click.
		a.Logger.Error("slack: team review could not be recorded", "review", id, "team", rv.Team, "error", err)
		if uerr := a.apiClient().chatUpdateBlocks(ctx, rv.Channel, ts, teamReviewUnrecordedNotice); uerr != nil {
			a.Logger.Warn("slack: team review unrecorded rewrite failed", "review", id, "error", uerr)
		}
		return channels.PostReceipt{}, fmt.Errorf("slack: record team review: %w", err)
	}
	return channels.PostReceipt{ID: id, Channel: rv.Channel, TS: ts}, nil
}

// newTeamReviewRecord is the record of review as posted now, before its
// message exists (TS is set once Slack names it).
func newTeamReviewRecord(review channels.TeamReview, id string) store.Review {
	return store.Review{
		ID:      id,
		Channel: review.Channel,
		Team:    review.Team,
		Text:    review.Text,
		Link:    review.Link,
		Tool:    review.Approve.Tool, Arguments: review.Approve.Arguments,
		PostedAt: time.Now(),
		TTL:      teamReviewTTL,
	}
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

// teamReviewBlocks renders an open review: the ask, the buttons and, when the
// record carries a status line, that line under them for the team to read.
func teamReviewBlocks(rv store.Review) []any {
	elements := []any{
		map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: "✅ Approve"},
			bkStyle:    bkPrimary,
			bkActionID: teamReviewApprove,
			bkValue:    encodeTeamReviewValue(rv.ID),
		},
	}
	if rv.Link != "" {
		elements = append(elements, map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: "Open PR"},
			bkActionID: teamReviewOpen,
			bkURL:      rv.Link,
			bkValue:    encodeTeamReviewValue(rv.ID),
		})
	}
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: teamHeading(rv.Team, rv.Text)}},
		map[string]any{bkType: bkActions, bkElements: elements},
	}
	if rv.Status != "" {
		blocks = append(blocks, contextBlock(rv.Status))
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
// is decided as the clicker. A store that does not answer tells the clicker
// to try again and leaves the message alone: not knowing is not "expired".
func (a *Adapter) handleTeamReviewDecision(ctx context.Context, slackChannel, messageTS, clicker, value string) {
	id, ok := decodeTeamReviewValue(value)
	if !ok {
		return
	}
	rv, found, err := a.reviews().GetReview(ctx, id)
	if err != nil {
		a.Logger.Warn("slack: team review lookup failed", "record", "team_review_store_failed", "review", id, "slack_user", clicker, "error", err)
		a.tellClickerIn(ctx, slackChannel, id, clicker, teamReviewUnavailableNotice)
		return
	}
	if !found {
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
// — reopens the review and is written under the buttons once, naming the
// clicker and the reason, where the clicker and the team both read it; so is
// a manager the gateway could not reach. A success is written into the
// message with the decider.
func (a *Adapter) decideTeamReview(ctx context.Context, rv store.Review, clicker string, resumed bool) {
	// The person's own token is the identity the tool runs under; without a
	// link there is nobody to act as, so the click is turned into a sign-in.
	token, ok, signIn := a.humanToken(ctx, rv.Channel, "", clicker)
	if signIn {
		a.postSignIn(ctx, rv.Channel, "", clicker, false)
	}
	if !ok {
		return
	}

	current, found, claimed, err := a.claimTeamReview(ctx, rv.ID, clicker)
	switch {
	case err != nil:
		a.Logger.Warn("slack: team review claim failed", "record", "team_review_store_failed", "review", rv.ID, "slack_user", clicker, "error", err)
		a.tellClicker(ctx, rv, clicker, teamReviewUnavailableNotice)
		return
	case !found:
		a.tellClicker(ctx, rv, clicker, teamReviewExpiredNotice)
		return
	case !claimed:
		notice := fmt.Sprintf(teamReviewDecidedNotice, current.DecidedBy)
		if !current.Done {
			notice = fmt.Sprintf(teamReviewPendingNotice, current.DecidedBy)
		}
		a.tellClicker(ctx, rv, clicker, notice)
		return
	}

	res, err := a.Tools.CallTool(ctx, token, rv.Tool, rv.Arguments)
	if err != nil {
		a.releaseTeamReview(ctx, rv.ID)
		a.Logger.Warn("slack: team review tool call failed", "review", rv.ID, "team", rv.Team, "tool", rv.Tool, "user", clicker, "error", err)
		a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusFailed, clicker))
		return
	}
	if server, loginURL, challenged := authChallengeOf(res); challenged {
		a.releaseTeamReview(ctx, rv.ID)
		if resumed {
			// The sign-in landed and the backend still challenges: do not loop
			// the person through the consent flow again on their behalf.
			a.Logger.Warn("slack: team review still challenged after the connector sign-in", "review", rv.ID, "team", rv.Team, "tool", rv.Tool, "user", clicker, "server", server)
			a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusStillChallenged, clicker, escapeMrkdwn(server)))
			return
		}
		a.promptTeamReviewConnect(ctx, rv, clicker, server, loginURL)
		return
	}
	if res.IsError {
		a.releaseTeamReview(ctx, rv.ID)
		a.Logger.Info("slack: team review approval refused by the tool", "record", "team_review_refused",
			"review", rv.ID, "team", rv.Team, "tool", rv.Tool, "slack_user", clicker, "reason", res.Text)
		reason := "the manager refused it"
		if res.Text != "" {
			reason = truncateRunes(escapeMrkdwn(res.Text), teamReviewReasonMax)
		}
		a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusRefused, clicker, reason))
		return
	}

	a.finishTeamReview(ctx, rv.ID)
	a.Logger.Info("slack: team review approved", "record", "team_review_approved",
		"review", rv.ID, "team", rv.Team, "tool", rv.Tool, "slack_user", clicker, "subject", a.linkedSubject(clicker), "resumed", resumed)
	if err := a.apiClient().chatUpdate(ctx, rv.Channel, rv.TS, teamReviewOutcome(rv, clicker, res.Text), teamReviewOutcomeBlocks(rv, clicker, res.Text)); err != nil {
		a.Logger.Warn("slack: team review outcome rewrite failed", "review", rv.ID, "error", err)
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
func (a *Adapter) promptTeamReviewConnect(ctx context.Context, rv store.Review, clicker, server, loginURL string) {
	a.Logger.Info("slack: team review needs the person to connect the backend", "record", "team_review_connect",
		"review", rv.ID, "team", rv.Team, "tool", rv.Tool, "slack_user", clicker, "server", server)
	a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusConnecting, clicker, escapeMrkdwn(server)))

	promptURL, connectValue := loginURL, server
	text := fmt.Sprintf(teamReviewConnectManualNotice, escapeMrkdwn(server))
	if base := a.PublicBaseURL; base != "" {
		stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: clicker, server: server, channel: rv.Channel, review: rv.ID})
		if decorated, err := decorateConnectorLoginURL(loginURL, base, stateID); err != nil {
			a.Logger.Warn("slack: team review login URL decoration failed, posting plain link", "review", rv.ID, "server", server, "error", err)
		} else {
			promptURL, connectValue = decorated, stateID
			text = fmt.Sprintf(teamReviewConnectNotice, escapeMrkdwn(server))
		}
	}
	if err := a.apiClient().postConnectPrompt(ctx, rv.Channel, "", clicker, text, server, promptURL, connectValue, false); err != nil {
		a.Logger.Warn("slack: team review connect prompt failed", "review", rv.ID, "user", clicker, "error", err)
	}
}

// resumeTeamReviewApproval is the landing's continuation of a Connect prompt
// posted for a review: the person signed in to the backend, so the approval
// they clicked for is submitted again as them. A review gone meanwhile (its
// TTL passed) is told to the person alone; so is a store that does not answer.
func (a *Adapter) resumeTeamReviewApproval(ctx context.Context, entry connectorCompletion) {
	rv, found, err := a.reviews().GetReview(ctx, entry.review)
	if err != nil {
		a.Logger.Warn("slack: team review lookup failed", "record", "team_review_store_failed", "review", entry.review, "slack_user", entry.slackUser, "error", err)
		a.tellClickerIn(ctx, entry.channel, entry.review, entry.slackUser, teamReviewUnavailableNotice)
		return
	}
	if !found {
		a.tellClickerIn(ctx, entry.channel, entry.review, entry.slackUser, teamReviewExpiredNotice)
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
func teamReviewOutcome(rv store.Review, decider, result string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ *Approved* by <@%s> for %s.\n%s", decider, escapeMrkdwn(rv.Team), rv.Text)
	if said := toolMessage(result); said != "" {
		fmt.Fprintf(&b, "\n_%s_", truncateRunes(escapeMrkdwn(said), teamReviewReasonMax))
	}
	return truncateRunes(b.String(), slackSectionTextMax)
}

// toolMessage is what of a tool's answer is shown to people: a plain text as
// written; of a JSON object its "message" field — a tool that answers agents
// with structured data puts the sentence for a channel there — and nothing of
// a JSON object without one. Structured data is for the caller, not the team.
func toolMessage(result string) string {
	result = strings.TrimSpace(result)
	if result == "" {
		return ""
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(result), &object); err != nil {
		if strings.HasPrefix(result, "[") || strings.HasPrefix(result, "{") {
			return "" // structured, but not an object with a message
		}
		return result
	}
	message, _ := object["message"].(string)
	return strings.TrimSpace(message)
}

// teamReviewOutcomeBlocks renders the approved review: the outcome, and the
// link kept as small print so the pull request stays one click away.
func teamReviewOutcomeBlocks(rv store.Review, decider, result string) []any {
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: teamReviewOutcome(rv, decider, result)}},
	}
	if rv.Link != "" {
		blocks = append(blocks, contextBlock(openLink(rv.Link)))
	}
	return blocks
}

// showTeamReviewStatus records status as the review's current status line and
// rewrites the message with it, buttons kept. A review decided meanwhile, or a
// store that cannot say, leaves the message alone: it must never be rewritten
// back to its buttons over an approval.
func (a *Adapter) showTeamReviewStatus(ctx context.Context, rv store.Review, status string) {
	if !a.setTeamReviewStatus(ctx, rv.ID, status) {
		return
	}
	rv.Status = status
	if err := a.apiClient().chatUpdate(ctx, rv.Channel, rv.TS, teamMessageFallback(rv.Team, rv.Text), teamReviewBlocks(rv)); err != nil {
		a.Logger.Warn("slack: team review status rewrite failed", "review", rv.ID, "error", err)
	}
}

// tellClicker answers a click on a review somebody else decided, visibly only
// to the clicker, in the channel where they clicked: the message itself says
// nothing new to them.
func (a *Adapter) tellClicker(ctx context.Context, rv store.Review, clicker, text string) {
	a.tellClickerIn(ctx, rv.Channel, rv.ID, clicker, text)
}

// tellClickerIn is tellClicker for a review the gateway holds no record of,
// located by the channel the click came from.
func (a *Adapter) tellClickerIn(ctx context.Context, channel, id, clicker, text string) {
	if err := a.apiClient().postEphemeralText(ctx, channel, clicker, "", text); err != nil {
		a.Logger.Warn("slack: team review notice failed", "review", id, "user", clicker, "error", err)
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

// --- review records: one atomic update of the store's record per transition ---

// reviews returns the store the review records live in: Adapter.Reviews, the
// gateway's store, or — for an adapter given none (tests, a gateway without a
// store) — an in-process memory store, so the records live for the life of
// the process as on --store=memory.
func (a *Adapter) reviews() store.ReviewStore {
	if a.Reviews != nil {
		return a.Reviews
	}
	a.reviewsMu.Lock()
	defer a.reviewsMu.Unlock()
	if a.memReviews == nil {
		a.memReviews = memory.New()
	}
	return a.memReviews
}

// claimTeamReview marks the review as being decided by user, as one atomic
// update of the record, so of two clicks — on one replica or two — exactly
// one takes it. It reports the record as it stands after the attempt: found
// is false for a review that is gone, claimed false when another approval is
// in flight or done (current.DecidedBy says whose). A claim without an
// outcome older than teamReviewClaimLease is taken over: the process that
// held it died mid-call.
func (a *Adapter) claimTeamReview(ctx context.Context, id, user string) (current store.Review, found, claimed bool, err error) {
	now := time.Now()
	found, err = a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		claimed = false
		held := r.DecidedBy != "" && (r.Done || now.Sub(r.ClaimedAt) <= teamReviewClaimLease)
		if held {
			current = *r
			return false
		}
		r.DecidedBy, r.ClaimedAt = user, now
		current, claimed = *r, true
		return true
	})
	return current, found, claimed, err
}

// releaseTeamReview reopens a review whose approval did not go through. A
// store that does not answer is logged: the lease frees the claim in time.
func (a *Adapter) releaseTeamReview(ctx context.Context, id string) {
	if _, err := a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		if r.Done {
			return false
		}
		r.DecidedBy, r.ClaimedAt = "", time.Time{}
		return true
	}); err != nil {
		a.Logger.Warn("slack: team review release failed", "record", "team_review_store_failed", "review", id, "error", err)
	}
}

// finishTeamReview records the approval as done; the record stays until its
// TTL so a late click is told who decided.
func (a *Adapter) finishTeamReview(ctx context.Context, id string) {
	if _, err := a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		r.Done, r.Status = true, ""
		return true
	}); err != nil {
		a.Logger.Warn("slack: team review finish failed", "record", "team_review_store_failed", "review", id, "error", err)
	}
}

// setTeamReviewStatus records the status line of an open review. It reports
// false for a review that is unknown or decided already — whose message must
// not be rewritten back to the buttons — and for a store that cannot say.
func (a *Adapter) setTeamReviewStatus(ctx context.Context, id, status string) bool {
	set := false
	found, err := a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		set = false
		if r.Done {
			return false
		}
		r.Status, set = status, true
		return true
	})
	if err != nil {
		a.Logger.Warn("slack: team review status update failed", "record", "team_review_store_failed", "review", id, "error", err)
		return false
	}
	return found && set
}
