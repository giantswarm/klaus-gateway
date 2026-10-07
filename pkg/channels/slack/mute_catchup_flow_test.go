package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// The turn that ends a mute hands the agent what the people wrote while the
// thread was muted. These flows mute thread 500.000 at 500.001, serve the
// thread from conversations.replies, and read the catch-up off the message
// that reaches the gateway.

// mutedThread is thread 500.000 as Slack holds it when U1 mentions the bot at
// 500.010: the root, the agent's answer, the mute and its note, what U1 and
// U2 said meanwhile, and the mention itself. Slack may return the root even
// with a later oldest, so it is served here.
func mutedThread() []replyMsg {
	return []replyMsg{
		{TS: "500.000", User: "U1", Text: "why is the cluster unhappy?"},
		{TS: "500.0005", User: "UBOT", Text: "ok"},
		{TS: "500.001", User: "U1", Text: "mute"},
		{TS: "500.0015", User: "UBOT", Text: mutedNote},
		{TS: "500.002", User: "U1", Text: "I think it is the disk"},
		{TS: "500.003", User: "U2", SubType: "channel_join", Text: "<@U2> has joined the channel"},
		{TS: "500.004", User: "U2", Text: "no, the network: <@U1> look at the drops"},
		{TS: "500.010", User: "U1", Text: "<@UBOT> what do you make of it?"},
	}
}

// A mention after the mute carries, under its own label, what was written in
// between, oldest first: the root, the mute, the bot's own posts, channel
// events and the mention itself are left out, and every page of the read
// starts at the mute.
func TestMuteCatchUp_MentionCarriesTheMutedPeriod(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(mutedThread(), 3)
	fake.withUserNames(map[string]string{"U1": "Jose", "U2": "Marta"}, nil)
	gw, dispatched := capturingGateway()
	a, srv, _ := startMutedThreadWith(t, fake, gw)

	sendEvent(t, srv, mention("U1", "<@UBOT> what do you make of it?", "500.010", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	msg := dispatched()[1]
	require.Equal(t, strings.Join([]string{
		"[messages written in this thread while the agent was muted: 2 messages, oldest first]",
		"1970-01-01 00:08 Jose: I think it is the disk",
		"1970-01-01 00:08 Marta: no, the network: Jose look at the drops",
	}, "\n"), msg.Context)
	require.Contains(t, msg.Text, "what do you make of it?", "the mention is the turn's own text")

	reads := fake.pathCalls("conversations.replies")
	require.Len(t, reads, 3, "the read pages through the whole thread")
	for _, r := range reads {
		require.Equal(t, "500.001", r.params["oldest"], "every page starts at the mute")
	}
}

// A mute with nothing written after it gives the turn only its own message:
// no label, and nobody is told anything.
func TestMuteCatchUp_NothingWrittenGivesNoLabel(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread([]replyMsg{
		{TS: "500.000", User: "U1", Text: "why is the cluster unhappy?"},
		{TS: "500.001", User: "U1", Text: "mute"},
		{TS: "500.0015", User: "UBOT", Text: mutedNote},
		{TS: "500.010", User: "U1", Text: "<@UBOT> back?"},
	}, 50)
	gw, dispatched := capturingGateway()
	a, srv, _ := startMutedThreadWith(t, fake, gw)
	ephemerals := len(fake.pathCalls("chat.postEphemeral"))

	sendEvent(t, srv, mention("U1", "<@UBOT> back?", "500.010", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	require.Empty(t, dispatched()[1].Context)
	require.Len(t, fake.pathCalls("chat.postEphemeral"), ephemerals)
}

// A read that fails ends the mute all the same and runs the turn without the
// catch-up; the person who mentioned the bot is told why.
func TestMuteCatchUp_ReadFailureRunsTheTurnAndNotifies(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.setFail("conversations.replies", "missing_scope")
	gw, dispatched := capturingGateway()
	a, srv, _ := startMutedThreadWith(t, fake, gw)

	sendEvent(t, srv, mention("U1", "<@UBOT> what do you make of it?", "500.010", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	require.Empty(t, dispatched()[1].Context)
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"), "the mute is over")
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")),
		"The messages written while the agent was muted could not be read (`missing_scope`)")
}

// A muted thread can hold an open approval: mute rejects the open card, and
// the turn that resumes the rejected task may ask for another. A mention that
// answers it ends the mute and reaches the agent as the decision alone, which
// carries no context, so the thread is not read for it.
func TestMuteCatchUp_AnswerToAnOpenPromptIsNotRead(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(mutedThread(), 50)
	gw, dispatched := capturingGateway()
	gw.sendQueue = [][]channels.OutboundDelta{
		{{Kind: channels.DeltaPrompt, TaskID: "task-1", Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete"}}},
		{{Kind: channels.DeltaPrompt, TaskID: "task-2", Prompt: &channels.HitlPrompt{ToolName: "kubectl_scale"}}},
		{{Content: "done"}, {Done: true}},
	}
	a, srv := openThread(t, gw, fake)
	waitThreadIdle(t, a, "500.000")
	sendEvent(t, srv, threadReply("U1", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond,
		"the mute rejects the first card, and the resumed turn asks for another")
	waitThreadIdle(t, a, "500.000")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"))

	sendEvent(t, srv, mention("U1", "<@UBOT> approve", "500.010", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 3 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	msg := dispatched()[2]
	require.Equal(t, "task-2", msg.TaskID)
	require.NotNil(t, msg.Decision, "the mention answers the open approval")
	require.Empty(t, msg.Context)
	require.Empty(t, fake.pathCalls("conversations.replies"), "the thread is not read for a decision")
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"), "the mute is over")
}

// Only the turn that ends the mute catches up: the next reply is a turn of
// the conversation, and a mentioned command word ends no mute and reads
// nothing.
func TestMuteCatchUp_OnlyTheUnmutingTurnReads(t *testing.T) {
	fake := newFakeSlackAPI()
	fake.withThread(mutedThread(), 50)
	gw, dispatched := capturingGateway()
	a, srv, _ := startMutedThreadWith(t, fake, gw)

	sendEvent(t, srv, mention("U1", "<@UBOT> help", "500.005", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "Commands: ")
	}, flowWait, 20*time.Millisecond)
	require.Empty(t, fake.pathCalls("conversations.replies"), "a command word reads nothing")

	sendEvent(t, srv, mention("U1", "<@UBOT> what do you make of it?", "500.010", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")
	require.Len(t, fake.pathCalls("conversations.replies"), 1)

	sendEvent(t, srv, threadReply("U1", "and the disk?", "500.011", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 3 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")
	require.Empty(t, dispatched()[2].Context, "a later turn carries no catch-up")
	require.Len(t, fake.pathCalls("conversations.replies"), 1, "and reads nothing")
}
