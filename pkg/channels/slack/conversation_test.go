package slack_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

func guideConversation() channels.Conversation {
	return channels.Conversation{
		Person: "u1@example.com",
		From:   "Your guide",
		Text:   "The supervisor asks whether kagent rolls onto gazelle **tonight**.",
		Reply:  channels.ToolInvocation{Tool: "x_beekeeper_send_message", Arguments: map[string]any{"to": "local:machine/Guide"}},
	}
}

// conversationHarness is a gateway whose Slack workspace has u1@example.com
// as U1, whose direct messages land in D1.
func conversationHarness(t *testing.T, tools *recordingTools, opts ...func(*slackadapter.Adapter)) (*slackadapter.Adapter, *httptest.Server, *fakeSlackAPI) {
	t.Helper()
	a, srv, fake := teamReviewHarness(t, tools, opts...)
	fake.setResponder("users.lookupByEmail", func(p map[string]any) string {
		if p["email"] == "u1@example.com" {
			return `{"ok":true,"user":{"id":"U1"}}`
		}
		return `{"ok":false,"error":"users_not_found"}`
	})
	fake.setResponder("chat.postMessage", func(map[string]any) string {
		return `{"ok":true,"channel":"D1","ts":"1700000100.000001"}`
	})
	return a, srv, fake
}

// replyInConversation sends a message of user's into the conversation's
// thread.
func replyInConversation(t *testing.T, srv *httptest.Server, receipt channels.PostReceipt, user, ts, text string) {
	t.Helper()
	sendEvent(t, srv, fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel_type":"im","user":%q,"text":%q,"channel":%q,"ts":%q,"thread_ts":%q,"parent_user_id":"UBOT"}}`,
		user, text, receipt.Channel, ts, receipt.TS))
}

// threadNotes are the texts of the gateway's notes in the thread at ts.
func threadNotes(fake *fakeSlackAPI, ts string) []string {
	var notes []string
	for _, p := range fake.pathCalls("chat.postMessage") {
		if p.params["thread_ts"] == ts {
			text, _ := p.params["text"].(string)
			notes = append(notes, text)
		}
	}
	return notes
}

func TestConversation_OpensADirectMessageFromTheAgent(t *testing.T) {
	a, _, fake := conversationHarness(t, &recordingTools{})

	receipt, err := a.OpenConversation(context.Background(), guideConversation())
	require.NoError(t, err)
	require.Equal(t, channels.PostReceipt{ID: "D1-1700000100.000001", Channel: "D1", TS: "1700000100.000001"}, receipt)

	post := fake.pathCalls("chat.postMessage")[0]
	require.Equal(t, "U1", post.params["channel"], "a direct message is addressed by user ID")
	blocks := blocksOf(post)
	require.Equal(t, "markdown", blocks[0]["type"])
	require.Equal(t, "The supervisor asks whether kagent rolls onto gazelle **tonight**.", blocks[0]["text"])
	require.Equal(t, "Your guide · reply in this thread", blocks[1]["elements"].([]any)[0].(map[string]any)["text"])

	c := guideConversation()
	c.Person = "nobody@example.com"
	_, err = a.OpenConversation(context.Background(), c)
	require.ErrorIs(t, err, channels.ErrAddresseeNotFound)
}

func TestConversation_PersonsReplyGoesToTheAgentAsThem(t *testing.T) {
	for name, s := range reviewStores(t) {
		t.Run(name, func(t *testing.T) {
			tools := &recordingTools{results: []muster.Result{{Text: "sent: delivery 4"}}}
			a, srv, fake := conversationHarness(t, tools, withReviews(s))
			receipt, err := a.OpenConversation(context.Background(), guideConversation())
			require.NoError(t, err)

			replyInConversation(t, srv, receipt, "U1", "1800000000.000002", "Roll it &amp; tell me")

			waitFor(t, "the reply is sent", func() bool { return len(tools.recorded()) == 1 })
			call := tools.recorded()[0]
			require.Equal(t, "id-token-U1", call.bearer, "the reply goes as the person")
			require.Equal(t, "x_beekeeper_send_message", call.tool)
			require.Equal(t, map[string]any{
				"to": "local:machine/Guide",
				"message": map[string]any{
					"messageId": "slack-D1-1800000000.000002",
					"role":      "user",
					"contextId": receipt.ID,
					"parts":     []any{map[string]any{"kind": "text", "text": "Roll it & tell me"}},
					"metadata":  map[string]any{"source": "slack", "conversation": receipt.ID},
				},
			}, call.args)
			waitFor(t, "the reply is marked delivered", func() bool {
				for _, r := range fake.pathCalls("reactions.add") {
					if r.params["timestamp"] == "1800000000.000002" && r.params["name"] == "incoming_envelope" {
						return true
					}
				}
				return false
			})
			require.Empty(t, threadNotes(fake, receipt.TS), "a delivered reply needs no note")

			// The same event delivered again is the same message, sent once.
			replyInConversation(t, srv, receipt, "U1", "1800000000.000002", "Roll it &amp; tell me")
			replyInConversation(t, srv, receipt, "U1", "1800000000.000003", "and the backup")
			waitFor(t, "the next reply is sent", func() bool { return len(tools.recorded()) == 2 })
			require.Equal(t, "slack-D1-1800000000.000003", tools.recorded()[1].args["message"].(map[string]any)["messageId"])
		})
	}
}

func TestConversation_RefusalSaysTheReplyWasNotDelivered(t *testing.T) {
	tools := &recordingTools{results: []muster.Result{{IsError: true, Text: "local:machine/Guide is not on the roster"}}}
	a, srv, fake := conversationHarness(t, tools)
	receipt, err := a.OpenConversation(context.Background(), guideConversation())
	require.NoError(t, err)

	replyInConversation(t, srv, receipt, "U1", "1800000000.000002", "Are you there?")

	waitFor(t, "the thread says why", func() bool {
		notes := threadNotes(fake, receipt.TS)
		return len(notes) == 1 && notes[0] == "Not delivered to *Your guide*: local:machine/Guide is not on the roster"
	})
	require.Empty(t, fake.pathCalls("reactions.add"))
}

func TestConversation_OnlyThePersonReachesTheAgent(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := conversationHarness(t, tools)
	receipt, err := a.OpenConversation(context.Background(), guideConversation())
	require.NoError(t, err)

	replyInConversation(t, srv, receipt, "U2", "1800000000.000002", "Let me in")

	waitFor(t, "the other person is told", func() bool {
		return ephemeralTo(fake, "U2", "This thread is <@U1>'s conversation with *Your guide*; only they can write to it.")
	})
	require.Empty(t, tools.recorded())
}

func TestConversation_UnlinkedPersonIsAskedToSignIn(t *testing.T) {
	tools := &recordingTools{}
	a, srv, fake := teamReviewHarness(t, tools)
	fake.setResponder("users.lookupByEmail", func(map[string]any) string { return `{"ok":true,"user":{"id":"U9"}}` })
	receipt, err := a.OpenConversation(context.Background(), guideConversation())
	require.NoError(t, err)

	replyInConversation(t, srv, receipt, "U9", "1800000000.000002", "Hello")

	waitFor(t, "the person gets the sign-in card", func() bool {
		for _, e := range fake.pathCalls("chat.postEphemeral") {
			if e.params["user"] == "U9" && strings.Contains(fmt.Sprint(e.params["blocks"]), "Sign in") {
				return true
			}
		}
		return false
	})
	require.Empty(t, tools.recorded())
}

func TestConversation_MessagesGoIntoTheThread(t *testing.T) {
	a, _, fake := conversationHarness(t, &recordingTools{})
	receipt, err := a.OpenConversation(context.Background(), guideConversation())
	require.NoError(t, err)

	posted, err := a.PostConversationMessage(context.Background(), receipt.ID, channels.ConversationMessage{Text: "Noted; I hand it to the supervisor."})
	require.NoError(t, err)
	require.Equal(t, receipt.ID, posted.ID)
	post := fake.pathCalls("chat.postMessage")[1]
	require.Equal(t, "D1", post.params["channel"])
	require.Equal(t, receipt.TS, post.params["thread_ts"])
	require.Equal(t, "Noted; I hand it to the supervisor.", blocksOf(post)[0]["text"])

	_, err = a.PostConversationMessage(context.Background(), "D1-1.000", channels.ConversationMessage{Text: "hi"})
	require.ErrorIs(t, err, channels.ErrConversationNotFound)
	decision := rollDecision()
	decision.Team, decision.Channel, decision.Person = "", "", "u1@example.com"
	fake.setResponder("chat.postMessage", func(map[string]any) string {
		return `{"ok":true,"channel":"D1","ts":"1700000200.000001"}`
	})
	d, err := a.PostDecision(context.Background(), decision)
	require.NoError(t, err)
	_, err = a.PostConversationMessage(context.Background(), d.ID, channels.ConversationMessage{Text: "hi"})
	require.ErrorIs(t, err, channels.ErrConversationNotFound, "a decision is not a conversation")
}
