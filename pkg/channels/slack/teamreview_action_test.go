package slack_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// The action's review: one change by one person (U1, the actor) over several
// targets, landing as several pull requests, decided by a second linked
// member — Approve as before, Deny with a typed reason to the manager's deny
// tool — with a notice to a second channel and the action's results posted
// back into the thread.

const (
	actionPR1 = "https://github.com/giantswarm/a-configs/pull/12"
	actionPR2 = "https://github.com/giantswarm/b-management-clusters/pull/7"
	actionRun = "https://github.com/giantswarm/platform-manager/actions/runs/4242"
)

func actionReview() channels.TeamReview {
	return channels.TeamReview{
		Team:          "team-bumblebee",
		Channel:       "C1",
		Text:          "*Enable* `agent-platform` on two installations for <@U1>.",
		Link:          actionRun,
		Actor:         "u1@example.com",
		PullRequests:  []string{actionPR1, actionPR2},
		Approve:       channels.ToolInvocation{Tool: "x_giantswarm-platform-manager_approve_action", Arguments: map[string]any{"action": "a1"}},
		Deny:          channels.ToolInvocation{Tool: "x_giantswarm-platform-manager_deny_action", Arguments: map[string]any{"action": "a1"}},
		NoticeChannel: "C2",
	}
}

// clickDeny posts a signed team_review_deny click; Slack answers it with the
// reason modal.
func clickDeny(t *testing.T, srv *httptest.Server, clicker, reviewID, messageTS string) {
	t.Helper()
	value, err := json.Marshal(map[string]any{"r": reviewID})
	require.NoError(t, err)
	postInteraction(t, srv, reviewClick(clicker, "team_review_deny", string(value), messageTS))
}

// submitDeny posts the signed view_submission of the reason modal, as Slack
// sends it when the member clicks Deny in it.
func submitDeny(t *testing.T, srv *httptest.Server, user, privateMetadata, reason string) {
	t.Helper()
	postInteraction(t, srv, map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": user},
		"view": map[string]any{
			"id":               "V1",
			"callback_id":      "team_review_deny",
			"private_metadata": privateMetadata,
			"state": map[string]any{"values": map[string]any{
				"team_review_deny_reason": map[string]any{"reason": map[string]any{"type": "plain_text_input", "value": reason}},
			}},
		},
	})
}

// sectionTexts lists the mrkdwn of every section block, in order.
func sectionTexts(blocks []map[string]any) []string {
	var texts []string
	for _, b := range blocks {
		if b["type"] != "section" {
			continue
		}
		text, _ := b["text"].(map[string]any)["text"].(string)
		texts = append(texts, text)
	}
	return texts
}

// buttonLabels lists the labels of every button in the blocks.
func buttonLabels(blocks []map[string]any) []string {
	var labels []string
	for _, b := range blocks {
		if b["type"] != "actions" {
			continue
		}
		for _, e := range b["elements"].([]any) {
			labels = append(labels, e.(map[string]any)["text"].(map[string]any)["text"].(string))
		}
	}
	return labels
}

func TestTeamReview_ActionRendersPullRequestsNoticeAndDeny(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})

	receipt, err := a.PostTeamReview(context.Background(), actionReview())
	require.NoError(t, err)
	require.Equal(t, "C1", receipt.Channel)
	require.NotEmpty(t, receipt.NoticeTS, "the receipt locates the notice")
	require.NotEqual(t, receipt.TS, receipt.NoticeTS)

	posts := fake.pathCalls("chat.postMessage")
	require.Len(t, posts, 2, "the notice and the review")

	notice := posts[0]
	require.Equal(t, "C2", notice.params["channel"], "the notice goes to the second channel first")
	noticeBlocks := blocksOf(notice)
	require.Empty(t, actionIDs(noticeBlocks), "the notice carries no buttons")
	require.Contains(t, sectionTexts(noticeBlocks)[0], "*Enable* `agent-platform`", "the same text")
	require.Contains(t, sectionTexts(noticeBlocks)[1], "<"+actionPR1+"|giantswarm/a-configs#12>", "the pull requests as links")

	review := posts[1]
	require.Equal(t, "C1", review.params["channel"])
	blocks := blocksOf(review)
	require.Equal(t, []string{"team_review_approve", "team_review_deny", "team_review_open"}, actionIDs(blocks))
	require.Equal(t, []string{"Approve", "Deny", "Open run"}, buttonLabels(blocks), "the link button reads the target's kind")
	texts := sectionTexts(blocks)
	require.Len(t, texts, 2, "the ask and the pull requests")
	require.Contains(t, texts[1], "• <"+actionPR1+"|giantswarm/a-configs#12>")
	require.Contains(t, texts[1], "• <"+actionPR2+"|giantswarm/b-management-clusters#7>")
}

func TestTeamReview_NoticeFailureFailsTheReviewBeforeItIsPosted(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})
	fake.failWith["chat.postMessage"] = "channel_not_found"

	_, err := a.PostTeamReview(context.Background(), actionReview())
	require.ErrorContains(t, err, "notice to C2")
	require.Len(t, fake.pathCalls("chat.postMessage"), 1, "nothing with buttons went up")
}

func TestTeamReview_ActorCannotApproveTheirOwnAction(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{Text: "approving reviews submitted on 2 pull requests"}}}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), actionReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "the actor's click is refused under the buttons", func() bool {
		return strings.Contains(statusLine(fake, receipt.TS), "<@U1>'s approval was not accepted: the action is theirs")
	})
	require.Empty(t, tools.recorded(), "the actor's approval never reaches the manager")
	require.Equal(t, []string{"team_review_approve", "team_review_deny", "team_review_open"}, latestButtons(fake, receipt.TS), "the review stays open")

	clickApprove(t, srv, "U2", receipt.ID, receipt.TS)
	waitFor(t, "a second person's approval lands", func() bool {
		return updatedWith(fake, receipt.TS, "Approved", "<@U2>", "approving reviews submitted")
	})
	calls := tools.recorded()
	require.Len(t, calls, 1)
	require.Equal(t, "id-token-U2", calls[0].bearer, "the call runs as the deciding member")
	require.Equal(t, "x_giantswarm-platform-manager_approve_action", calls[0].tool)
	require.Empty(t, latestButtons(fake, receipt.TS))
}

func TestTeamReview_DenyWithReasonCallsTheDenyToolAsTheMember(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{Text: `{"message":"3 pull requests closed","closed":3}`}}}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), actionReview())
	require.NoError(t, err)

	clickDeny(t, srv, "U2", receipt.ID, receipt.TS)
	view := openedView(t, fake)
	require.Equal(t, "team_review_deny", view["callback_id"])
	require.Equal(t, "trigger-U2", fake.pathCalls("views.open")[0].params["trigger_id"], "opened for the clicker")
	pm, _ := view["private_metadata"].(string)
	require.Contains(t, pm, receipt.ID, "the modal remembers the review")
	require.Empty(t, tools.recorded(), "nothing is called before the reason is typed")

	submitDeny(t, srv, "U2", pm, "The customer asked to wait for their change freeze to end.")
	waitFor(t, "the deny tool is called once", func() bool { return len(tools.recorded()) == 1 })
	call := tools.recorded()[0]
	require.Equal(t, "id-token-U2", call.bearer, "the denial runs under the denying member's own token")
	require.Equal(t, "x_giantswarm-platform-manager_deny_action", call.tool)
	require.Equal(t, map[string]any{"action": "a1", "reason": "The customer asked to wait for their change freeze to end."}, call.args, "the deny arguments plus the typed reason")

	waitFor(t, "the message shows the denial, the decider and the reason", func() bool {
		return updatedWith(fake, receipt.TS, "Denied", "<@U2>", "change freeze", "3 pull requests closed")
	})
	require.Empty(t, latestButtons(fake, receipt.TS), "a denied review carries no buttons")
	require.Contains(t, sectionTexts(blocksOf(fake.pathCalls("chat.update")[len(fake.pathCalls("chat.update"))-1]))[1], "giantswarm/a-configs#12", "the pull requests stay listed")

	clickApprove(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "a later click is told who denied", func() bool {
		return ephemeralTo(fake, "U1", "already denied by <@U2>")
	})
	require.Len(t, tools.recorded(), 1)
}

func TestTeamReview_ActorMayDenyTheirOwnAction(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{Text: "withdrawn"}}}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), actionReview())
	require.NoError(t, err)

	clickDeny(t, srv, "U1", receipt.ID, receipt.TS)
	view := openedView(t, fake)
	submitDeny(t, srv, "U1", view["private_metadata"].(string), "Wrong installation, I'll run it again.")
	waitFor(t, "the actor's denial withdraws the action", func() bool {
		return updatedWith(fake, receipt.TS, "Denied", "<@U1>", "Wrong installation")
	})
	require.Equal(t, "id-token-U1", tools.recorded()[0].bearer)
}

func TestTeamReview_DenyRefusedByTheToolReopensTheReview(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{IsError: true, Text: "not a member of team-bumblebee"}}}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), actionReview())
	require.NoError(t, err)

	clickDeny(t, srv, "U2", receipt.ID, receipt.TS)
	view := openedView(t, fake)
	submitDeny(t, srv, "U2", view["private_metadata"].(string), "no")
	waitFor(t, "the refusal names the denial", func() bool {
		return strings.Contains(statusLine(fake, receipt.TS), "<@U2>'s denial was not accepted: not a member of team-bumblebee")
	})
	require.Equal(t, []string{"team_review_approve", "team_review_deny", "team_review_open"}, latestButtons(fake, receipt.TS), "the buttons stay")
}

func TestTeamReview_UnlinkedDenyClickIsAskedToSignIn(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), actionReview())
	require.NoError(t, err)

	clickDeny(t, srv, "U9", receipt.ID, receipt.TS)
	waitFor(t, "a sign-in prompt is posted for the unlinked clicker", func() bool {
		for _, path := range []string{"chat.postMessage", "chat.postEphemeral"} {
			for _, c := range fake.pathCalls(path) {
				if raw, _ := json.Marshal(c.params); strings.Contains(string(raw), signInLink) {
					return true
				}
			}
		}
		return false
	})
	require.Empty(t, fake.pathCalls("views.open"), "no modal for somebody with nobody to act as")
	require.Empty(t, tools.recorded())
}

func TestTeamReview_DenyClickOnADecidedReviewIsToldWhoDecided(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostTeamReview(context.Background(), actionReview())
	require.NoError(t, err)

	clickApprove(t, srv, "U2", receipt.ID, receipt.TS)
	waitFor(t, "U2 approves", func() bool { return updatedWith(fake, receipt.TS, "Approved", "<@U2>") })

	clickDeny(t, srv, "U1", receipt.ID, receipt.TS)
	waitFor(t, "the late Deny click is told who approved", func() bool { return ephemeralTo(fake, "U1", "already approved by <@U2>") })
	require.Empty(t, fake.pathCalls("views.open"), "no modal over a decision")
	require.Len(t, tools.recorded(), 1)
}

func TestTeamReview_ResultIsPostedIntoTheThread(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})
	receipt, err := a.PostTeamReview(context.Background(), actionReview())
	require.NoError(t, err)

	result, err := a.PostTeamReviewResult(context.Background(), receipt.ID, channels.TeamReviewResult{
		Text: "✅ Merged and rolled out on both installations; every probe green.", Link: actionRun,
	})
	require.NoError(t, err)
	require.Equal(t, receipt.ID, result.ID)
	require.Equal(t, "C1", result.Channel)
	require.NotEmpty(t, result.TS)

	posts := fake.pathCalls("chat.postMessage")
	require.Len(t, posts, 3, "the notice, the review, the result")
	follow := posts[2]
	require.Equal(t, "C1", follow.params["channel"])
	require.Equal(t, receipt.TS, follow.params["thread_ts"], "the result is a reply in the review's thread")
	blocks := blocksOf(follow)
	require.Contains(t, sectionTexts(blocks)[0], "every probe green")
	require.Contains(t, blocks[1]["elements"].([]any)[0].(map[string]any)["text"], "<"+actionRun+"|Open run>")

	_, err = a.PostTeamReviewResult(context.Background(), "no-such-review", channels.TeamReviewResult{Text: "merged"})
	require.ErrorIs(t, err, channels.ErrReviewNotFound)
}

// The link button and the inline link read what their URL points to (#289).
func TestTeamNotice_LinkReadsTheTargetsKind(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})
	cases := map[string]string{
		"https://github.com/giantswarm/github/pull/4711":                     "Open PR",
		"https://github.com/giantswarm/github/actions/runs/123/job/456":      "Open run",
		"https://github.com/giantswarm/github/issues/9":                      "Open issue",
		"https://github.com/giantswarm/github":                               "Open repository",
		"https://devportal.example/catalog/default/component/agent-platform": "Open link",
	}
	for link, label := range cases {
		_, err := a.PostTeamNotice(context.Background(), channels.TeamNotice{Team: "team-bumblebee", Channel: "C1", Text: "done", Link: link})
		require.NoError(t, err)
		posts := fake.pathCalls("chat.postMessage")
		blocks := blocksOf(posts[len(posts)-1])
		require.Equal(t, "<"+link+"|"+label+">", blocks[1]["elements"].([]any)[0].(map[string]any)["text"], link)
	}
}
