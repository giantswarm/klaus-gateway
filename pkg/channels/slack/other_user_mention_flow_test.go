package slack_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// channelReply is a plain message.channels thread reply, without the
// app_mention twin Slack only sends when the bot is mentioned.
func channelReply(user, text, ts, threadTS string) string {
	return fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel_type":"channel","user":%q,"text":%q,"channel":"C1","ts":%q,"thread_ts":%q}}`, user, text, ts, threadTS)
}

// A reply in the agent's thread that opens by mentioning another person talks
// to that person: the agent does not answer it and nothing is posted. A reply
// that mentions someone later, or that mentions the bot anywhere, still
// reaches the agent.
func TestHandleInbound_ReplyOpeningWithOtherUserMention(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "answer-text"}, {Done: true}}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "start", "700.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 20*time.Millisecond, "the mention starts the thread")
	waitThreadIdle(t, a, "700.000")
	posts := len(fake.pathCalls("chat.postMessage"))

	for i, text := range []string{"<@U2> can you check this?", "  <@W2|alice> over to you"} {
		sendEvent(t, srv, channelReply("U1", text, fmt.Sprintf("70%d.000", i+1), "700.000"))
	}
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.dispatchCount(), "a reply opening with another user's mention must not reach the agent")
	require.Len(t, fake.pathCalls("chat.postMessage"), posts, "nothing is posted for it")
	require.Empty(t, fake.pathCalls("chat.postEphemeral"))

	for i, text := range []string{"can you ask <@U2> about it?", "<@U2> <@UBOT> please look too"} {
		waitThreadIdle(t, a, "700.000")
		want := gw.dispatchCount() + 1
		sendEvent(t, srv, channelReply("U1", text, fmt.Sprintf("71%d.000", i), "700.000"))
		require.Eventually(t, func() bool { return gw.dispatchCount() == want },
			flowWait, 20*time.Millisecond, "%q still reaches the agent", text)
	}
}

// A reply to someone else does not answer the agent's paused question; the
// question stays open for the reply meant for it.
func TestHandleInbound_OtherUserMentionLeavesQuestionPending(t *testing.T) {
	fake := newFakeSlackAPI()
	prompt := &channels.HitlPrompt{
		ToolName:  channels.AskUserToolName,
		Questions: []channels.HitlQuestion{{Question: "Which cluster?", Choices: []string{"gazelle", "graveler"}}},
	}
	gw := &stubGateway{
		sendQueue: [][]channels.OutboundDelta{
			{{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: prompt}},
			{{Content: "graveler it is"}, {Done: true}},
		},
	}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "check a cluster", "800.000", ""))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Which cluster?")
	}, flowWait, 20*time.Millisecond, "the question is posted")
	waitThreadIdle(t, a, "800.000")

	sendEvent(t, srv, channelReply("U1", "<@U2> which one do you use?", "801.000", "800.000"))
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.dispatchCount(), "a reply to another user must not answer the question")

	sendEvent(t, srv, channelReply("U1", "graveler", "802.000", "800.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
		flowWait, 20*time.Millisecond, "the question is still pending for the answer meant for it")
}
