package slack_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

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
		sendEvent(t, srv, threadReply("U1", text, fmt.Sprintf("70%d.000", i+1), "700.000"))
	}
	require.Never(t, func() bool {
		return gw.dispatchCount() > 1 || len(fake.pathCalls("chat.postMessage")) > posts ||
			len(fake.pathCalls("chat.postEphemeral")) > 0
	}, 500*time.Millisecond, 20*time.Millisecond,
		"a reply opening with another user's mention must not reach the agent nor post anything")

	for i, text := range []string{"can you ask <@U2> about it?", "<@U2> <@UBOT> please look too"} {
		waitThreadIdle(t, a, "700.000")
		want := gw.dispatchCount() + 1
		sendEvent(t, srv, threadReply("U1", text, fmt.Sprintf("71%d.000", i), "700.000"))
		require.Eventually(t, func() bool { return gw.dispatchCount() == want },
			flowWait, 20*time.Millisecond, "%q still reaches the agent", text)
	}
}

// A reply to someone else does not answer the agent's paused question or
// approval; the prompt stays open for the reply meant for it, which resumes
// the paused task.
func TestHandleInbound_OtherUserMentionLeavesPromptPending(t *testing.T) {
	for name, tc := range map[string]struct {
		prompt *channels.HitlPrompt
		posted string
		answer string
	}{
		"question": {
			prompt: &channels.HitlPrompt{
				ToolName:  channels.AskUserToolName,
				Questions: []channels.HitlQuestion{{Question: "Which cluster?", Choices: []string{"gazelle", "graveler"}}},
			},
			posted: "Which cluster?",
			answer: "graveler",
		},
		"approval": {
			prompt: &channels.HitlPrompt{ToolName: "kube_delete", StatusText: "Delete the pod?"},
			posted: "*Approval required*",
			answer: "approve",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeSlackAPI()
			var mu sync.Mutex
			var dispatched []channels.InboundMessage
			gw := &stubGateway{
				sendQueue: [][]channels.OutboundDelta{
					{{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: tc.prompt, Content: tc.prompt.StatusText}},
					{{Content: "done"}, {Done: true}},
				},
				onDispatch: func(msg channels.InboundMessage) {
					mu.Lock()
					defer mu.Unlock()
					dispatched = append(dispatched, msg)
				},
			}
			a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

			sendEvent(t, srv, mention("U1", "do the thing", "800.000", ""))
			require.Eventually(t, func() bool {
				return strings.Contains(allText(fake.pathCalls("chat.postMessage")), tc.posted)
			}, flowWait, 20*time.Millisecond, "the prompt is posted")
			waitThreadIdle(t, a, "800.000")

			sendEvent(t, srv, threadReply("U1", "<@U2> what do you think?", "801.000", "800.000"))
			require.Never(t, func() bool { return gw.dispatchCount() > 1 },
				500*time.Millisecond, 20*time.Millisecond, "a reply to another user must not answer the prompt")

			sendEvent(t, srv, threadReply("U1", tc.answer, "802.000", "800.000"))
			require.Eventually(t, func() bool { return gw.dispatchCount() == 2 },
				flowWait, 20*time.Millisecond, "the answer meant for the prompt is dispatched")
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, "task-1", dispatched[1].TaskID, "the answer resumes the still-pending task")
			require.NotNil(t, dispatched[1].Decision)
		})
	}
}

// In a thread whose conversation ended, a reply to another person is not
// for the bot either, so it gets no conversation-ended notice.
func TestClosedThread_OtherUserMentionGetsNoNotice(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	rec, advance := agingRecorder(t)
	gw.records = rec
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "900.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "900.000")
	before := len(fake.pathCalls("chat.postEphemeral"))

	advance(channels.DefaultThreadTTL + time.Hour)

	sendEvent(t, srv, threadReply("U1", "<@U2> did you see this?", "900.001", "900.000"))
	require.Never(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) > before },
		500*time.Millisecond, 20*time.Millisecond, "a reply to another user gets no notice")
	require.Equal(t, 1, gw.dispatchCount())
}
