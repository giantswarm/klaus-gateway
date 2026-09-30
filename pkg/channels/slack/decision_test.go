package slack_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

func rollDecision() channels.Decision {
	return channels.Decision{
		Team:      "team-bumblebee",
		Channel:   "C1",
		Note:      "614",
		Question:  "Roll kagent v1.1.1 onto gazelle tonight?",
		StatusQuo: "graveler and glean run v1.1.1 since Tuesday.",
		Options: []store.DecisionOption{
			{Label: "Roll tonight", Consequence: "The lane clears at 22:00."},
			{Label: "Wait for Monday", Consequence: "Nothing rolls before Monday."},
		},
		Recommend: 2,
		Due:       time.Now().Add(time.Hour),
		Default:   "Wait for Monday.",
		AskedBy:   "Board pull 99",
		Answer:    channels.ToolInvocation{Tool: "x_beekeeper_note_answer", Arguments: map[string]any{"note": "614"}},
	}
}

// clickDecision posts a click on one of the decision message's buttons.
func clickDecision(t *testing.T, srv *httptest.Server, clicker, actionID, value string, receipt channels.PostReceipt) {
	t.Helper()
	postInteraction(t, srv, map[string]any{
		"type":       "block_actions",
		"trigger_id": "trigger-" + clicker,
		"user":       map[string]any{"id": clicker},
		"channel":    map[string]any{"id": receipt.Channel},
		"container":  map[string]any{"message_ts": receipt.TS},
		"message":    map[string]any{"ts": receipt.TS},
		"actions":    []any{map[string]any{"action_id": actionID, "value": value}},
	})
}

// latestUpdateText is the fallback text and every block's text of the latest
// rewrite of the message at ts, joined; "" when it was never rewritten.
func latestUpdateText(fake *fakeSlackAPI, ts string) string {
	updates := fake.pathCalls("chat.update")
	for i := len(updates) - 1; i >= 0; i-- {
		if updates[i].params["ts"] != ts {
			continue
		}
		var b strings.Builder
		for _, block := range blocksOf(updates[i]) {
			fmt.Fprintf(&b, "%v\n", block)
		}
		return b.String()
	}
	return ""
}

func TestDecision_RendersOptionsRecommendationAndDue(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})

	receipt, err := a.PostDecision(context.Background(), rollDecision())
	require.NoError(t, err)
	require.Equal(t, "C1-"+receipt.TS, receipt.ID, "a decision is named by its message")

	posts := fake.pathCalls("chat.postMessage")
	require.Len(t, posts, 1)
	require.Equal(t, "C1", posts[0].params["channel"])
	blocks := blocksOf(posts[0])
	require.Equal(t, "header", blocks[0]["type"])
	require.Equal(t, "Roll kagent v1.1.1 onto gazelle tonight?", blocks[0]["text"].(map[string]any)["text"])
	require.Contains(t, blocks[1]["text"].(map[string]any)["text"], "since Tuesday")

	first, second := blocks[2], blocks[3]
	require.Contains(t, first["text"].(map[string]any)["text"], "*Roll tonight*\nThe lane clears at 22:00.")
	require.NotContains(t, first["text"].(map[string]any)["text"], "Recommended")
	require.Nil(t, first["accessory"].(map[string]any)["style"])
	require.Equal(t, "1", first["accessory"].(map[string]any)["value"])
	require.True(t, strings.HasPrefix(second["text"].(map[string]any)["text"].(string), "_Recommended_\n*Wait for Monday*"))
	require.Equal(t, "primary", second["accessory"].(map[string]any)["style"])
	require.Equal(t, "decision_choose", second["accessory"].(map[string]any)["action_id"])

	require.Equal(t, []string{"decision_own_words"}, actionIDs(blocks))
	due := blocks[5]["elements"].([]any)[0].(map[string]any)["text"].(string)
	require.Contains(t, due, "<!date^")
	require.Contains(t, due, "if unanswered: Wait for Monday.")
	asked := blocks[6]["elements"].([]any)[0].(map[string]any)["text"].(string)
	require.Equal(t, "For team-bumblebee · Asked by Board pull 99 · note #614", asked)
}

func TestDecision_PersonIsReachedByEmailInADirectMessage(t *testing.T) {
	a, srv, fake := teamReviewHarness(t, &recordingTools{})
	fake.setResponder("users.lookupByEmail", func(p map[string]any) string {
		if p["email"] == "u1@example.com" {
			return `{"ok":true,"user":{"id":"U1"}}`
		}
		return `{"ok":false,"error":"users_not_found"}`
	})
	fake.setResponder("chat.postMessage", func(p map[string]any) string {
		return fmt.Sprintf(`{"ok":true,"channel":"D1","ts":"1700000100.000001","sent_to":%q}`, p["channel"])
	})
	d := rollDecision()
	d.Team, d.Channel, d.Person = "", "", "u1@example.com"

	receipt, err := a.PostDecision(context.Background(), d)
	require.NoError(t, err)
	require.Equal(t, "U1", fake.pathCalls("chat.postMessage")[0].params["channel"], "a direct message is addressed by user ID")
	require.Equal(t, "D1", receipt.Channel, "edits go to the conversation Slack put it in")
	require.Equal(t, "D1-1700000100.000001", receipt.ID)
	asked := blocksOf(fake.pathCalls("chat.postMessage")[0])[6]["elements"].([]any)[0].(map[string]any)["text"].(string)
	require.Equal(t, "Asked by Board pull 99 · note #614", asked)

	tools := &recordingTools{}
	a.Tools = tools
	clickDecision(t, srv, "U1", "decision_choose", "1", receipt)
	waitFor(t, "the answer is submitted", func() bool { return len(tools.recorded()) == 1 })
	waitFor(t, "the DM is rewritten in its conversation", func() bool {
		return strings.Contains(latestUpdateText(fake, receipt.TS), "Answered by <@U1>")
	})
	require.Equal(t, "D1", fake.pathCalls("chat.update")[0].params["channel"])

	d.Person = "nobody@example.com"
	_, err = a.PostDecision(context.Background(), d)
	require.ErrorIs(t, err, channels.ErrAddresseeNotFound)
}

func TestDecision_ChooseAnswersAsTheClickerOnce(t *testing.T) {
	for name, s := range reviewStores(t) {
		t.Run(name, func(t *testing.T) {
			tools := &recordingTools{results: []muster.Result{{Text: `{"state":"answered"}`}}}
			a, srv, fake := teamReviewHarness(t, tools, withReviews(s))
			receipt, err := a.PostDecision(context.Background(), rollDecision())
			require.NoError(t, err)

			clickDecision(t, srv, "U1", "decision_choose", "2", receipt)

			waitFor(t, "the answer tool is called", func() bool { return len(tools.recorded()) == 1 })
			call := tools.recorded()[0]
			require.Equal(t, "id-token-U1", call.bearer, "the answer runs as the person who clicked")
			require.Equal(t, "x_beekeeper_note_answer", call.tool)
			require.Equal(t, map[string]any{"note": "614", "choice": 2}, call.args)
			waitFor(t, "the message shows who answered what", func() bool {
				text := latestUpdateText(fake, receipt.TS)
				return strings.Contains(text, "Answered by <@U1>") && strings.Contains(text, "*Wait for Monday*")
			})
			require.Empty(t, latestButtons(fake, receipt.TS), "an answered decision has no buttons")

			clickDecision(t, srv, "U2", "decision_choose", "1", receipt)
			waitFor(t, "a second answer is told who answered", func() bool {
				return ephemeralTo(fake, "U2", "already answered by <@U1>")
			})
			require.Len(t, tools.recorded(), 1)
		})
	}
}

func TestDecision_RefusalIsAStatusLineAndTheDecisionStaysOpen(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{IsError: true, Text: "u2@example.com is not the addressee"}, {Text: "ok"}}}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostDecision(context.Background(), rollDecision())
	require.NoError(t, err)

	clickDecision(t, srv, "U2", "decision_choose", "1", receipt)
	waitFor(t, "the refusal is under the buttons", func() bool {
		return strings.Contains(latestUpdateText(fake, receipt.TS), "<@U2>'s answer was not accepted: u2@example.com is not the addressee")
	})
	require.Equal(t, []string{"decision_own_words"}, latestButtons(fake, receipt.TS), "the decision stays open")

	clickDecision(t, srv, "U1", "decision_choose", "1", receipt)
	waitFor(t, "another person answers", func() bool {
		return strings.Contains(latestUpdateText(fake, receipt.TS), "Answered by <@U1>")
	})
}

func TestDecision_OwnWordsModalAnswersWithTextAndOption(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostDecision(context.Background(), rollDecision())
	require.NoError(t, err)

	clickDecision(t, srv, "U1", "decision_own_words", "", receipt)
	waitFor(t, "the modal opens", func() bool { return len(fake.pathCalls("views.open")) == 1 })
	view := fake.pathCalls("views.open")[0].params["view"].(map[string]any)
	require.Equal(t, "decision_answer", view["callback_id"])
	require.Equal(t, receipt.ID, view["private_metadata"])
	choice := view["blocks"].([]any)[2].(map[string]any)
	require.Equal(t, true, choice["optional"], "the option is optional")
	require.Len(t, choice["element"].(map[string]any)["options"], 2)

	postInteraction(t, srv, map[string]any{
		"type": "view_submission",
		"user": map[string]any{"id": "U1"},
		"view": map[string]any{
			"id":               "V1",
			"callback_id":      "decision_answer",
			"private_metadata": receipt.ID,
			"state": map[string]any{"values": map[string]any{
				"decision_answer_text":   map[string]any{"text": map[string]any{"type": "plain_text_input", "value": " Roll, but only after the 21:00 backup. "}},
				"decision_answer_choice": map[string]any{"choice": map[string]any{"type": "static_select", "selected_option": map[string]any{"value": "1"}}},
			}},
		},
	})

	waitFor(t, "the answer is submitted", func() bool { return len(tools.recorded()) == 1 })
	require.Equal(t, map[string]any{"note": "614", "choice": 1, "text": "Roll, but only after the 21:00 backup."}, tools.recorded()[0].args)
	waitFor(t, "the message shows the option and the words", func() bool {
		return strings.Contains(latestUpdateText(fake, receipt.TS), "*Roll tonight* — Roll, but only after the 21:00 backup.")
	})
}

func TestDecision_ThreadReplyIsAnAnswerInOwnWords(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostDecision(context.Background(), rollDecision())
	require.NoError(t, err)

	// A reply under another bot message is not looked up as a decision.
	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","channel_type":"channel","user":"U1","text":"unrelated","channel":"C1","ts":"1800000000.000001","thread_ts":"1700000000.999999","parent_user_id":"UBOT"}}`)
	sendEvent(t, srv, fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel_type":"channel","user":"U1","text":"Wait &amp; see &lt;tomorrow&gt;","channel":"C1","ts":"1800000000.000002","thread_ts":%q,"parent_user_id":"UBOT"}}`, receipt.TS))

	waitFor(t, "the reply is submitted as the answer", func() bool { return len(tools.recorded()) == 1 })
	require.Equal(t, map[string]any{"note": "614", "text": "Wait & see <tomorrow>"}, tools.recorded()[0].args, "the words as the person typed them")
	waitFor(t, "the message shows the answer", func() bool {
		return strings.Contains(latestUpdateText(fake, receipt.TS), "Answered by <@U1>")
	})
	require.Len(t, tools.recorded(), 1, "the unrelated reply answered nothing")
}

func TestDecision_CloseRewritesAndRefusesLaterAnswers(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostDecision(context.Background(), rollDecision())
	require.NoError(t, err)

	closed, err := a.CloseDecision(context.Background(), receipt.ID, channels.DecisionClose{Outcome: store.DecisionDefaulted})
	require.NoError(t, err)
	require.Equal(t, receipt.TS, closed.TS)
	require.Contains(t, latestUpdateText(fake, receipt.TS), "the default was applied: Wait for Monday.")
	require.Empty(t, latestButtons(fake, receipt.TS))

	clickDecision(t, srv, "U1", "decision_choose", "1", receipt)
	waitFor(t, "a late click is told how it closed", func() bool {
		return ephemeralTo(fake, "U1", "not answered in time")
	})
	require.Empty(t, tools.recorded())

	_, err = a.CloseDecision(context.Background(), "C1-1.000", channels.DecisionClose{Outcome: store.DecisionWithdrawn})
	require.ErrorIs(t, err, channels.ErrDecisionNotFound)
	review, err := a.PostTeamReview(context.Background(), archiveReview())
	require.NoError(t, err)
	_, err = a.CloseDecision(context.Background(), review.ID, channels.DecisionClose{Outcome: store.DecisionWithdrawn})
	require.ErrorIs(t, err, channels.ErrDecisionNotFound, "a review is not a decision")
}

func TestDecision_AnsweredElsewhereShowsTheClosesText(t *testing.T) {
	a, _, fake := teamReviewHarness(t, &recordingTools{})
	receipt, err := a.PostDecision(context.Background(), rollDecision())
	require.NoError(t, err)

	_, err = a.CloseDecision(context.Background(), receipt.ID, channels.DecisionClose{Outcome: store.DecisionAnswered, Text: "Wait for Monday (timo, in the terminal)"})
	require.NoError(t, err)
	text := latestUpdateText(fake, receipt.TS)
	require.Contains(t, text, "Answered · <!date^")
	require.Contains(t, text, ": Wait for Monday (timo, in the terminal)")
}

// A person who never linked is asked to sign in, and nothing is submitted.
func TestDecision_UnlinkedClickerIsAskedToSignIn(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	receipt, err := a.PostDecision(context.Background(), rollDecision())
	require.NoError(t, err)

	clickDecision(t, srv, "U9", "decision_choose", "1", receipt)
	waitFor(t, "the clicker gets the sign-in card", func() bool {
		for _, e := range fake.pathCalls("chat.postEphemeral") {
			if e.params["user"] == "U9" && strings.Contains(fmt.Sprint(e.params["blocks"]), "Sign in") {
				return true
			}
		}
		return false
	})
	require.Empty(t, tools.recorded())
}
