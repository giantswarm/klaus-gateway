package slack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// The team review is the prompt kind a manager posts into a team's channel
// through POST /reviews (pkg/reviews): the change spelled out, the pull
// requests it lands as, an Approve button, a Deny button when the manager
// names a deny tool, and a link for anything else. It has no thread
// initiator; the decision rule is the team's — any Slack user with a linked
// identity may click, and the click calls the review's tool as that person.
// Membership of the named team is the tool's to check (it holds the person's
// GitHub grant); a refusal there leaves the review open for another member.
// A review that names its actor is an action's approval: the actor's own
// Approve is refused here — a second person decides — while their Deny
// withdraws the action. One decision closes it: a later click is told who
// decided.
//
// A review is a channel message, not a thread. What happened to the latest
// attempt that did not decide it — who is connecting, whose approval the
// manager refused and why, whose could not be submitted — is a status line
// on the message itself, replaced on every attempt and gone once the review
// is decided: the clicker and the team read it in the same place, once. A
// channel-level ephemeral, which Slack shows right where they clicked, is
// reserved for what is the clicker's alone: a sign-in or Connect button, or a
// click on a review somebody else decided. The action's results, which the
// manager posts through POST /reviews/{id}/results, are replies in the
// message's thread, so one thread carries the whole action.
//
// The record — the ask, where its message is, the decision state and the
// status line (store.Review) — lives in the gateway's store (Adapter.Reviews)
// for the review's TTL, so on a store that outlives the process a review
// posted before a restart is decided by a click after it. Every transition
// (claim, release, finish, status) is one atomic update of the record, and
// the claim is what makes one decision close the review across replicas.

// Team-review action IDs.
const (
	teamReviewApprove = "team_review_approve" // the Approve button
	teamReviewDeny    = "team_review_deny"    // the Deny button; opens the reason modal
	teamReviewOpen    = "team_review_open"    // the URL button; the browser opens it, no handling
)

// The Deny modal: its callback_id, and the block and action the typed reason
// is read from on submission.
const (
	teamReviewDenyCallbackID     = "team_review_deny"
	teamReviewDenyReasonBlockID  = "team_review_deny_reason"
	teamReviewDenyReasonActionID = "reason"
	teamReviewDenyTitle          = "Deny"
	teamReviewDenyReasonLabel    = "Why is this denied?"
	teamReviewDenyReasonHint     = "The reason is passed to the manager and shown to the team."
	teamReviewDenySubmitLabel    = "Deny"
	teamReviewDenyCloseLabel     = "Cancel"
)

// teamReviewTTL bounds how long an undecided review stays clickable. A click
// past it rewrites the message to say so; the manager may post the ask again.
const teamReviewTTL = 7 * 24 * time.Hour

// teamReviewClaimLease bounds how long a claim without an outcome holds the
// review. A click's tool call is bounded by the muster client's timeout (a
// minute), so a claim older than this was left behind by a process that died
// mid-call — a restart during the call — and the next click may take the
// review over instead of being told for seven days that a decision is in
// flight.
const teamReviewClaimLease = 5 * time.Minute

// Team-review notices to one clicker.
const (
	teamReviewExpiredNotice  = "_This review has expired. Ask for it to be posted again._"
	teamReviewApprovedNotice = "This review was already approved by <@%s>."
	teamReviewDeniedNotice   = "This review was already denied by <@%s>."
	teamReviewPendingNotice  = "<@%s>'s %s is being submitted right now."
	// teamReviewUnavailableNotice answers a click the gateway could not
	// resolve because its store did not answer; the message keeps its buttons.
	teamReviewUnavailableNotice = "_The review could not be looked up right now. Click the button again in a moment._"
	// teamReviewUnrecordedNotice replaces a review the gateway posted but
	// could not record: its button would never resolve.
	teamReviewUnrecordedNotice = "_This review could not be recorded and cannot be decided here. Ask for it to be posted again._"
	// teamReviewConnectNotice heads the Connect prompt when the sign-in lands
	// back on the gateway and the decision is resubmitted by itself;
	// teamReviewConnectManualNotice when it does not. The verbs are the
	// decision's: approve/approval, deny/denial.
	teamReviewConnectNotice       = "To %s as yourself, connect *%s* once. Your %s is submitted as soon as you're back."
	teamReviewConnectManualNotice = "To %s as yourself, connect *%s* once, then click *%s* again."
)

// Team-review status lines, under the buttons, read by the clicker and the
// team alike. Each replaces the previous one; the decision's outcome replaces
// them all. The noun is the decision's: approval or denial.
const (
	teamReviewStatusConnecting      = "🔗 <@%s> is connecting *%s* to %s as themselves."
	teamReviewStatusRefused         = "❌ <@%s>'s %s was not accepted: %s"
	teamReviewStatusFailed          = "⚠️ <@%s>'s %s could not be submitted to the manager; another click may do."
	teamReviewStatusStillChallenged = "⚠️ <@%s> connected *%s*, but the manager still asks them to sign in; another click may do."
	teamReviewStatusActor           = "❌ <@%s>'s approval was not accepted: the action is theirs, a second person decides."
)

// teamReviewReasonMax bounds a tool's text — its refusal in the status line,
// its answer in the outcome — and the reason a member types into the Deny
// modal.
const teamReviewReasonMax = 500

// teamReviewUnknownServer names the backend in a Connect prompt when muster's
// challenge does not.
const teamReviewUnknownServer = "the manager"

// ToolCaller calls a muster tool as the person whose bearer token it is
// given. *muster.Client satisfies it.
type ToolCaller interface {
	CallTool(ctx context.Context, bearer, tool string, args map[string]any) (muster.Result, error)
}

// teamReviewDecision is what a click decides: an approval, or a denial with
// the reason the member typed.
type teamReviewDecision struct {
	deny   bool
	reason string
}

func (d teamReviewDecision) verb() string {
	if d.deny {
		return "deny"
	}
	return "approve"
}

func (d teamReviewDecision) noun() string {
	if d.deny {
		return "denial"
	}
	return "approval"
}

// button is the label of the button the decision is made with.
func (d teamReviewDecision) button() string {
	if d.deny {
		return "Deny"
	}
	return "Approve"
}

// call is the tool call the decision makes on rv: the approve tool with its
// arguments, or the deny tool with its arguments and the reason.
func (d teamReviewDecision) call(rv store.Review) (tool string, args map[string]any) {
	if !d.deny {
		return rv.Tool, rv.Arguments
	}
	args = maps.Clone(rv.DenyArguments)
	if args == nil {
		args = map[string]any{}
	}
	args["reason"] = d.reason
	return rv.DenyTool, args
}

// teamReviewValue is the value of the review's buttons and the Deny modal's
// private metadata: the review id, and for the modal the channel the click
// came from, so a submission the store cannot resolve can still be answered
// where the person clicked.
type teamReviewValue struct {
	Review  string `json:"r"`
	Channel string `json:"c,omitempty"`
}

func encodeTeamReviewValue(id string) string {
	return encodeTeamReviewValueIn(id, "")
}

func encodeTeamReviewValueIn(id, channel string) string {
	b, _ := json.Marshal(teamReviewValue{Review: id, Channel: channel})
	return string(b)
}

func decodeTeamReviewValue(raw string) (teamReviewValue, bool) {
	var v teamReviewValue
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v.Review == "" {
		return teamReviewValue{}, false
	}
	return v, true
}

func newTeamReviewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// PostTeamReview posts the ask into its channel and records it so a click can
// be resolved. A review naming a second channel posts the notice there
// first: a notice that cannot be posted fails the request before anything
// with buttons is up, and a notice about an ask that then fails to post is
// harmless where a second ask is not. It implements channels.TeamReviewPoster.
func (a *Adapter) PostTeamReview(ctx context.Context, review channels.TeamReview) (channels.PostReceipt, error) {
	if a.Tools == nil || a.OBO == nil {
		return channels.PostReceipt{}, errors.New("slack: team reviews need a tool caller and account linking")
	}
	id, err := newTeamReviewID()
	if err != nil {
		return channels.PostReceipt{}, fmt.Errorf("slack: team review id: %w", err)
	}
	var noticeTS string
	if review.NoticeChannel != "" {
		notice, err := a.PostTeamNotice(ctx, channels.TeamNotice{
			Team: review.Team, Channel: review.NoticeChannel, Text: review.Text, Link: review.Link, PullRequests: review.PullRequests,
		})
		if err != nil {
			return channels.PostReceipt{}, fmt.Errorf("slack: notice to %s: %w", review.NoticeChannel, err)
		}
		noticeTS = notice.TS
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
		// The message is up but nothing will resolve its buttons: say so in
		// its place rather than leave a button that reads "expired" on the
		// first click.
		a.Logger.Error("slack: team review could not be recorded", "review", id, "team", rv.Team, "error", err)
		if uerr := a.apiClient().chatUpdateBlocks(ctx, rv.Channel, ts, teamReviewUnrecordedNotice); uerr != nil {
			a.Logger.Warn("slack: team review unrecorded rewrite failed", "review", id, "error", uerr)
		}
		return channels.PostReceipt{}, fmt.Errorf("slack: record team review: %w", err)
	}
	return channels.PostReceipt{ID: id, Channel: rv.Channel, TS: ts, NoticeTS: noticeTS}, nil
}

// newTeamReviewRecord is the record of review as posted now, before its
// message exists (TS is set once Slack names it).
func newTeamReviewRecord(review channels.TeamReview, id string) store.Review {
	return store.Review{
		ID:           id,
		Channel:      review.Channel,
		Team:         review.Team,
		Text:         review.Text,
		Link:         review.Link,
		Actor:        strings.TrimSpace(review.Actor),
		PullRequests: review.PullRequests,
		Tool:         review.Approve.Tool, Arguments: review.Approve.Arguments,
		DenyTool: review.Deny.Tool, DenyArguments: review.Deny.Arguments,
		PostedAt: time.Now(),
		TTL:      teamReviewTTL,
	}
}

// PostTeamNotice posts a message that asks for nothing: no buttons, the pull
// requests and the link (when given) inline. It implements
// channels.TeamReviewPoster.
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

// PostTeamReviewResult posts an action's outcome as a reply in the review's
// thread. It implements channels.TeamReviewPoster; a review the gateway holds
// no record of is channels.ErrReviewNotFound.
func (a *Adapter) PostTeamReviewResult(ctx context.Context, id string, result channels.TeamReviewResult) (channels.PostReceipt, error) {
	rv, found, err := a.reviews().GetReview(ctx, id)
	if err != nil {
		return channels.PostReceipt{}, fmt.Errorf("slack: look up team review: %w", err)
	}
	if !found {
		return channels.PostReceipt{}, channels.ErrReviewNotFound
	}
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: truncateRunes(result.Text, slackSectionTextMax)}},
	}
	if result.Link != "" {
		blocks = append(blocks, contextBlock(openLink(result.Link)))
	}
	ts, err := a.apiClient().postJSON(ctx, methodChatPostMessage, map[string]any{
		paramChannel:  rv.Channel,
		paramThreadTS: rv.TS,
		paramText:     teamMessageFallback(rv.Team, result.Text),
		paramBlocks:   blocks,
	})
	if err != nil {
		return channels.PostReceipt{}, err
	}
	a.Logger.Info("slack: team review result posted", "record", "team_review_result_posted", "review", id, "team", rv.Team, "channel", rv.Channel, "ts", ts)
	return channels.PostReceipt{ID: id, Channel: rv.Channel, TS: ts}, nil
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

// teamReviewBlocks renders an open review: the ask, the pull requests, the
// buttons and, when the record carries a status line, that line under them
// for the team to read.
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
	if rv.DenyTool != "" {
		elements = append(elements, map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: "❌ Deny"},
			bkStyle:    bkDanger,
			bkActionID: teamReviewDeny,
			bkValue:    encodeTeamReviewValue(rv.ID),
		})
	}
	if rv.Link != "" {
		elements = append(elements, map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: linkLabel(rv.Link)},
			bkActionID: teamReviewOpen,
			bkURL:      rv.Link,
			bkValue:    encodeTeamReviewValue(rv.ID),
		})
	}
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: teamHeading(rv.Team, rv.Text)}},
	}
	blocks = appendPullRequests(blocks, rv.PullRequests)
	blocks = append(blocks, map[string]any{bkType: bkActions, bkElements: elements})
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
	blocks = appendPullRequests(blocks, notice.PullRequests)
	if notice.Link != "" {
		blocks = append(blocks, contextBlock(openLink(notice.Link)))
	}
	return blocks
}

// appendPullRequests adds the pull requests as one section of links, one per
// line, named by repository and number; nothing for none.
func appendPullRequests(blocks []any, pullRequests []string) []any {
	if len(pullRequests) == 0 {
		return blocks
	}
	lines := make([]string, 0, len(pullRequests))
	for _, pr := range pullRequests {
		lines = append(lines, fmt.Sprintf("• <%s|%s>", pr, escapeMrkdwn(pullRequestLabel(pr))))
	}
	return append(blocks, map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: truncateRunes(strings.Join(lines, "\n"), slackSectionTextMax)}})
}

// GitHub URL shapes the link and pull-request labels read.
var (
	githubPullPath  = regexp.MustCompile(`^/([^/]+)/([^/]+)/pull/(\d+)/?$`)
	githubIssuePath = regexp.MustCompile(`^/[^/]+/[^/]+/issues/\d+/?$`)
	githubRunPath   = regexp.MustCompile(`^/[^/]+/[^/]+/actions/runs/\d+(/.*)?$`)
	repositoryPath  = regexp.MustCompile(`^/[^/]+/[^/]+/?$`)
)

// pullRequestLabel names a pull request by repository and number
// (owner/repo#n) when its URL has GitHub's shape, and by the URL itself
// otherwise.
func pullRequestLabel(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	if m := githubPullPath.FindStringSubmatch(u.Path); m != nil {
		return fmt.Sprintf("%s/%s#%s", m[1], m[2], m[3])
	}
	return link
}

// linkLabel names the link button by what its URL points to: a pull request,
// a workflow run, an issue, a repository, or a link.
func linkLabel(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return "Open link"
	}
	switch {
	case githubPullPath.MatchString(u.Path):
		return "Open PR"
	case githubRunPath.MatchString(u.Path):
		return "Open run"
	case githubIssuePath.MatchString(u.Path):
		return "Open issue"
	case repositoryPath.MatchString(u.Path):
		return "Open repository"
	}
	return "Open link"
}

// openLink is the link of a review, notice or result as inline mrkdwn.
func openLink(link string) string {
	return fmt.Sprintf("<%s|%s>", link, linkLabel(link))
}

// lookupTeamReview resolves a click's value against the review record. An
// unknown or expired review rewrites the dead buttons; a store that does not
// answer tells the clicker to try again and leaves the message alone: not
// knowing is not "expired". ok is true only for a review the click may go
// on with.
func (a *Adapter) lookupTeamReview(ctx context.Context, slackChannel, messageTS, clicker, id string) (store.Review, bool) {
	rv, found, err := a.reviews().GetReview(ctx, id)
	if err != nil {
		a.Logger.Warn("slack: team review lookup failed", "record", "team_review_store_failed", "review", id, "slack_user", clicker, "error", err)
		a.tellClickerIn(ctx, slackChannel, id, clicker, teamReviewUnavailableNotice)
		return store.Review{}, false
	}
	if !found {
		if err := a.apiClient().chatUpdateBlocks(ctx, slackChannel, messageTS, teamReviewExpiredNotice); err != nil {
			a.Logger.Warn("slack: team review expired rewrite failed", "review", id, "error", err)
		}
		return store.Review{}, false
	}
	if a.Tools == nil {
		a.Logger.Error("slack: team review click without a tool caller", "review", id)
		return store.Review{}, false
	}
	return rv, true
}

// handleTeamReviewDecision resolves an Approve click against the review
// record and decides the review as the clicker.
func (a *Adapter) handleTeamReviewDecision(ctx context.Context, slackChannel, messageTS, clicker, value string) {
	v, ok := decodeTeamReviewValue(value)
	if !ok {
		return
	}
	rv, ok := a.lookupTeamReview(ctx, slackChannel, messageTS, clicker, v.Review)
	if !ok {
		return
	}
	a.decideTeamReview(ctx, rv, clicker, teamReviewDecision{}, false)
}

// handleTeamReviewDenyClick answers a Deny click with the reason modal. The
// clicker must be linked (an unlinked one is asked to sign in) and the review
// still open (a click on a decided one is told who decided); the claim is
// taken on submission, so a modal left open holds nothing. Everything before
// views.open shares Slack's three-second trigger budget: a store read and a
// cached token.
func (a *Adapter) handleTeamReviewDenyClick(ctx context.Context, slackChannel, messageTS, clicker, triggerID, value string) {
	v, ok := decodeTeamReviewValue(value)
	if !ok {
		return
	}
	rv, ok := a.lookupTeamReview(ctx, slackChannel, messageTS, clicker, v.Review)
	if !ok || rv.DenyTool == "" {
		return
	}
	if decided := a.tellIfDecided(ctx, rv, clicker); decided {
		return
	}
	_, ok, signIn := a.humanToken(ctx, rv.Channel, "", clicker)
	if signIn {
		a.postSignIn(ctx, rv.Channel, "", clicker, false, signInForClick)
	}
	if !ok {
		return
	}
	if err := a.apiClient().viewsOpen(ctx, triggerID, teamReviewDenyView(rv)); err != nil {
		a.Logger.Warn("slack: team review deny modal failed", "review", rv.ID, "user", clicker, "error", err)
		a.tellClicker(ctx, rv, clicker, teamReviewUnavailableNotice)
	}
}

// teamReviewDenyView is the Deny modal: one required reason box, the review
// in the private metadata.
func teamReviewDenyView(rv store.Review) map[string]any {
	reason := map[string]any{
		bkType:        bkPlainTextInput,
		bkActionID:    teamReviewDenyReasonActionID,
		bkMultiline:   true,
		bkMaxLength:   teamReviewReasonMax,
		bkPlaceholder: plainTextObj(teamReviewDenyReasonHint),
	}
	return map[string]any{
		bkType:            bkModal,
		bkCallbackID:      teamReviewDenyCallbackID,
		bkPrivateMetadata: encodeTeamReviewValueIn(rv.ID, rv.Channel),
		bkTitle:           plainTextObj(teamReviewDenyTitle),
		bkSubmit:          plainTextObj(teamReviewDenySubmitLabel),
		bkClose:           plainTextObj(teamReviewDenyCloseLabel),
		bkBlocks: []any{
			map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: teamHeading(rv.Team, rv.Text)}},
			map[string]any{bkType: bkInput, bkBlockID: teamReviewDenyReasonBlockID, bkLabel: plainTextObj(teamReviewDenyReasonLabel), bkElement: reason},
		},
	}
}

// handleTeamReviewDenial decides the review as the member who submitted the
// Deny modal, with the reason they typed. The modal is closed by the time
// this runs; what the person is told goes where they clicked.
func (a *Adapter) handleTeamReviewDenial(ctx context.Context, payload interactionPayload) {
	v, ok := decodeTeamReviewValue(payload.View.PrivateMetadata)
	if !ok {
		return
	}
	reason := strings.TrimSpace(payload.View.State.Values[teamReviewDenyReasonBlockID][teamReviewDenyReasonActionID].Value)
	if reason == "" {
		// Slack requires the input before it submits; a submission without
		// it did not come from the modal.
		return
	}
	clicker := payload.User.ID
	rv, found, err := a.reviews().GetReview(ctx, v.Review)
	if err != nil {
		a.Logger.Warn("slack: team review lookup failed", "record", "team_review_store_failed", "review", v.Review, "slack_user", clicker, "error", err)
		a.tellClickerIn(ctx, v.Channel, v.Review, clicker, teamReviewUnavailableNotice)
		return
	}
	if !found {
		a.tellClickerIn(ctx, v.Channel, v.Review, clicker, teamReviewExpiredNotice)
		return
	}
	if a.Tools == nil || rv.DenyTool == "" {
		a.Logger.Error("slack: team review denial without a deny tool", "review", rv.ID)
		return
	}
	a.decideTeamReview(ctx, rv, clicker, teamReviewDecision{deny: true, reason: truncateRunes(reason, teamReviewReasonMax)}, false)
}

// decideTeamReview submits the decision as clicker: the clicker must have a
// linked identity (an unlinked one is asked to sign in, the review stays
// open), an approval must not be the actor's own, the review must still be
// open (a second click is told who decided), and the decision's tool is
// called as the clicker.
//
// What the tool answers decides the rest. A sign-in challenge — muster does
// not hold the person's grant for the tool's backend yet — turns into a
// Connect prompt for the clicker; the sign-in lands back on the gateway and
// the decision is submitted again (resumed=true), so the person connects once
// and never clicks twice. A refusal — the manager finding the person outside
// the team, the author of their own change, or anything else it will not do
// — reopens the review and is written under the buttons once, naming the
// clicker and the reason, where the clicker and the team both read it; so is
// a manager the gateway could not reach. A success is written into the
// message with the decider.
func (a *Adapter) decideTeamReview(ctx context.Context, rv store.Review, clicker string, decision teamReviewDecision, resumed bool) {
	// The person's own token is the identity the tool runs under; without a
	// link there is nobody to act as, so the click is turned into a sign-in.
	token, ok, signIn := a.humanToken(ctx, rv.Channel, "", clicker)
	if signIn {
		a.postSignIn(ctx, rv.Channel, "", clicker, false, signInForClick)
	}
	if !ok {
		return
	}
	// A decided review answers every click the same way, the actor's too.
	if a.tellIfDecided(ctx, rv, clicker) {
		return
	}
	if !decision.deny && a.isTeamReviewActor(rv, clicker) {
		a.Logger.Info("slack: team review approval by the actor refused", "record", "team_review_actor_refused",
			"review", rv.ID, "team", rv.Team, "slack_user", clicker, "subject", a.linkedSubject(clicker))
		a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusActor, clicker))
		return
	}

	current, found, claimed, err := a.claimTeamReview(ctx, rv.ID, clicker, decision.deny)
	switch {
	case err != nil:
		a.Logger.Warn("slack: team review claim failed", "record", "team_review_store_failed", "review", rv.ID, "slack_user", clicker, "error", err)
		a.tellClicker(ctx, rv, clicker, teamReviewUnavailableNotice)
		return
	case !found:
		a.tellClicker(ctx, rv, clicker, teamReviewExpiredNotice)
		return
	case !claimed:
		a.tellClicker(ctx, rv, clicker, decidedNotice(current))
		return
	}

	tool, args := decision.call(rv)
	res, err := a.Tools.CallTool(ctx, token, tool, args)
	if err != nil {
		a.releaseTeamReview(ctx, rv.ID)
		a.Logger.Warn("slack: team review tool call failed", "review", rv.ID, "team", rv.Team, "tool", tool, "user", clicker, "error", err)
		a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusFailed, clicker, decision.noun()))
		return
	}
	if server, loginURL, challenged := authChallengeOf(res); challenged {
		a.releaseTeamReview(ctx, rv.ID)
		if resumed {
			// The sign-in landed and the backend still challenges: do not loop
			// the person through the consent flow again on their behalf.
			a.Logger.Warn("slack: team review still challenged after the connector sign-in", "review", rv.ID, "team", rv.Team, "tool", tool, "user", clicker, "server", server)
			a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusStillChallenged, clicker, escapeMrkdwn(server)))
			return
		}
		a.promptTeamReviewConnect(ctx, rv, clicker, decision, server, loginURL)
		return
	}
	if res.IsError {
		a.releaseTeamReview(ctx, rv.ID)
		a.Logger.Info("slack: team review decision refused by the tool", "record", "team_review_refused",
			"review", rv.ID, "team", rv.Team, "tool", tool, "slack_user", clicker, "denial", decision.deny, "reason", res.Text)
		reason := "the manager refused it"
		if res.Text != "" {
			reason = truncateRunes(escapeMrkdwn(res.Text), teamReviewReasonMax)
		}
		a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusRefused, clicker, decision.noun(), reason))
		return
	}

	a.finishTeamReview(ctx, rv.ID, decision.deny)
	a.Logger.Info("slack: team review decided", "record", decision.record(),
		"review", rv.ID, "team", rv.Team, "tool", tool, "slack_user", clicker, "subject", a.linkedSubject(clicker), "resumed", resumed)
	if err := a.apiClient().chatUpdate(ctx, rv.Channel, rv.TS, teamReviewOutcome(rv, clicker, decision, res.Text), teamReviewOutcomeBlocks(rv, clicker, decision, res.Text)); err != nil {
		a.Logger.Warn("slack: team review outcome rewrite failed", "review", rv.ID, "error", err)
	}
}

// record names the log record of a decision that went through.
func (d teamReviewDecision) record() string {
	if d.deny {
		return "team_review_denied"
	}
	return "team_review_approved"
}

// isTeamReviewActor reports whether clicker is the person whose action the
// review decides: the review names the actor by email, and the clicker's
// linked identity carries the email their Slack account was linked to. A
// review without an actor, or an identity source that exposes none, names
// nobody; the manager's own author check under the person's GitHub grant
// stands regardless.
func (a *Adapter) isTeamReviewActor(rv store.Review, clicker string) bool {
	if rv.Actor == "" {
		return false
	}
	ident, ok := a.OBO.(linkedIdentitySource)
	if !ok {
		return false
	}
	_, email, linked := ident.LinkedIdentity(clicker)
	return linked && email != "" && strings.EqualFold(email, rv.Actor)
}

// tellIfDecided answers a click on a decided review with who decided, or on
// one whose decision is in flight with whose; it reports whether it did.
func (a *Adapter) tellIfDecided(ctx context.Context, rv store.Review, clicker string) bool {
	held := rv.DecidedBy != "" && (rv.Done || time.Since(rv.ClaimedAt) <= teamReviewClaimLease)
	if !held {
		return false
	}
	a.tellClicker(ctx, rv, clicker, decidedNotice(rv))
	return true
}

// decidedNotice tells a clicker who decided the review, or whose decision is
// in flight.
func decidedNotice(rv store.Review) string {
	switch {
	case !rv.Done:
		return fmt.Sprintf(teamReviewPendingNotice, rv.DecidedBy, teamReviewDecision{deny: rv.Denied}.noun())
	case rv.Denied:
		return fmt.Sprintf(teamReviewDeniedNotice, rv.DecidedBy)
	}
	return fmt.Sprintf(teamReviewApprovedNotice, rv.DecidedBy)
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
// gateway's landing and the completion state remembers the review and the
// decision, so the landing submits it again as the person; without one the
// prompt asks them to click the button again afterwards.
func (a *Adapter) promptTeamReviewConnect(ctx context.Context, rv store.Review, clicker string, decision teamReviewDecision, server, loginURL string) {
	a.Logger.Info("slack: team review needs the person to connect the backend", "record", "team_review_connect",
		"review", rv.ID, "team", rv.Team, "tool", rv.Tool, "slack_user", clicker, "server", server, "denial", decision.deny)
	a.showTeamReviewStatus(ctx, rv, fmt.Sprintf(teamReviewStatusConnecting, clicker, escapeMrkdwn(server), decision.verb()))

	promptURL, connectValue := loginURL, server
	text := fmt.Sprintf(teamReviewConnectManualNotice, decision.verb(), escapeMrkdwn(server), decision.button())
	if base := a.PublicBaseURL; base != "" {
		stateID := a.mintConnectorCompletion(connectorCompletion{slackUser: clicker, server: server, channel: rv.Channel, review: rv.ID, decision: decision})
		if decorated, err := decorateConnectorLoginURL(loginURL, base, stateID); err != nil {
			a.Logger.Warn("slack: team review login URL decoration failed, posting plain link", "review", rv.ID, "server", server, "error", err)
		} else {
			promptURL, connectValue = decorated, stateID
			text = fmt.Sprintf(teamReviewConnectNotice, decision.verb(), escapeMrkdwn(server), decision.noun())
		}
	}
	if err := a.apiClient().postConnectPrompt(ctx, rv.Channel, "", clicker, text, server, promptURL, connectValue, false); err != nil {
		a.Logger.Warn("slack: team review connect prompt failed", "review", rv.ID, "user", clicker, "error", err)
	}
}

// resumeTeamReviewDecision is the landing's continuation of a Connect prompt
// posted for a review: the person signed in to the backend, so the decision
// they clicked for is submitted again as them. A review gone meanwhile (its
// TTL passed) is told to the person alone; so is a store that does not answer.
func (a *Adapter) resumeTeamReviewDecision(ctx context.Context, entry connectorCompletion) {
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
	a.decideTeamReview(ctx, rv, entry.slackUser, entry.decision, true)
}

// teamReviewOutcome is the text the review message is rewritten to once it is
// decided: who decided and how, for which team, the ask, the reason of a
// denial, and what the tool said.
func teamReviewOutcome(rv store.Review, decider string, decision teamReviewDecision, result string) string {
	var b strings.Builder
	if decision.deny {
		fmt.Fprintf(&b, "❌ *Denied* by <@%s> for %s.\n%s\n> %s", decider, escapeMrkdwn(rv.Team), rv.Text, escapeMrkdwn(decision.reason))
	} else {
		fmt.Fprintf(&b, "✅ *Approved* by <@%s> for %s.\n%s", decider, escapeMrkdwn(rv.Team), rv.Text)
	}
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

// teamReviewOutcomeBlocks renders the decided review: the outcome, the pull
// requests, and the link kept as small print so the change stays one click
// away.
func teamReviewOutcomeBlocks(rv store.Review, decider string, decision teamReviewDecision, result string) []any {
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: teamReviewOutcome(rv, decider, decision, result)}},
	}
	blocks = appendPullRequests(blocks, rv.PullRequests)
	if rv.Link != "" {
		blocks = append(blocks, contextBlock(openLink(rv.Link)))
	}
	return blocks
}

// showTeamReviewStatus records status as the review's current status line and
// rewrites the message with it, buttons kept. A review decided meanwhile, or a
// store that cannot say, leaves the message alone: it must never be rewritten
// back to its buttons over a decision.
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
// is false for a review that is gone, claimed false when another decision is
// in flight or done (current.DecidedBy says whose, current.Denied which). A
// claim without an outcome older than teamReviewClaimLease is taken over: the
// process that held it died mid-call.
func (a *Adapter) claimTeamReview(ctx context.Context, id, user string, deny bool) (current store.Review, found, claimed bool, err error) {
	now := time.Now()
	found, err = a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		claimed = false
		held := r.DecidedBy != "" && (r.Done || now.Sub(r.ClaimedAt) <= teamReviewClaimLease)
		if held {
			current = *r
			return false
		}
		r.DecidedBy, r.ClaimedAt, r.Denied = user, now, deny
		current, claimed = *r, true
		return true
	})
	return current, found, claimed, err
}

// releaseTeamReview reopens a review whose decision did not go through. A
// store that does not answer is logged: the lease frees the claim in time.
func (a *Adapter) releaseTeamReview(ctx context.Context, id string) {
	if _, err := a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		if r.Done {
			return false
		}
		r.DecidedBy, r.ClaimedAt, r.Denied = "", time.Time{}, false
		return true
	}); err != nil {
		a.Logger.Warn("slack: team review release failed", "record", "team_review_store_failed", "review", id, "error", err)
	}
}

// finishTeamReview records the decision as done; the record stays until its
// TTL so a late click is told who decided, and how.
func (a *Adapter) finishTeamReview(ctx context.Context, id string, denied bool) {
	if _, err := a.reviews().UpdateReview(ctx, id, func(r *store.Review) bool {
		r.Done, r.Denied, r.Status = true, denied, ""
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
