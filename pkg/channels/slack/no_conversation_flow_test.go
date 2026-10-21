package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// A conversation opens only through the agent picker. A message that finds no
// conversation in its thread is answered with the help, led by the line that
// says so, and starts nothing.

// helpReplies returns the help replies posted in threadID: the messages led
// by the no-conversation line.
func helpReplies(fake *fakeSlackAPI, threadID string) []recordedCall {
	var out []recordedCall
	for _, c := range fake.pathCalls("chat.postMessage") {
		if c.params["thread_ts"] == threadID && strings.HasPrefix(fmt.Sprint(c.params["text"]), "No conversation is open here") {
			out = append(out, c)
		}
	}
	return out
}

// A mention on a channel message, and one under a thread the agent is not in,
// get the help in their own thread: no turn runs and no row is written, so the
// shortcut can still open the thread. The help names the slash command.
func TestNoConversation_MentionGetsTheHelpInItsThread(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "<@UBOT> how are the clusters?", "300.000", ""))
	require.Eventually(t, func() bool { return len(helpReplies(fake, "300.000")) == 1 },
		flowWait, 20*time.Millisecond, "the help is posted in the mention's own thread")
	reply := helpReplies(fake, "300.000")[0]
	require.Equal(t, "C1", reply.params["channel"])
	require.Contains(t, fmt.Sprint(reply.params["text"]), "/swarmgeist [question]", "the fallback names the command")
	blocks, err := json.Marshal(reply.params["blocks"])
	require.NoError(t, err)
	require.Contains(t, string(blocks), "No conversation is open here, so your message did not reach an agent.")
	require.Contains(t, string(blocks), "/swarmgeist [question]")
	require.Contains(t, string(blocks), "Ask an agent here")
	require.Zero(t, gw.dispatchCount(), "nothing is dispatched")
	_, ok, err := gw.rec().ThreadRecord(context.Background(), "slack", "C1", "300.000")
	require.NoError(t, err)
	require.False(t, ok, "nothing is written: the thread stays free for the picker")

	// A mention under an alert's thread, which the agent is not in, the same.
	sendEvent(t, srv, mention("U2", "<@UBOT> what is this?", "401.000", "400.000"))
	require.Eventually(t, func() bool { return len(helpReplies(fake, "400.000")) == 1 },
		flowWait, 20*time.Millisecond, "the help is posted in the alert's thread")
	require.Zero(t, gw.dispatchCount())
	require.Empty(t, fake.pathCalls("conversations.replies"), "a thread nobody opened a conversation in is not read")
}

// A new chat in the direct message gets the help too: its first message names
// no conversation, in the thread Slack anchored for the chat. The help names
// the slash command as configured.
func TestNoConversation_NewChatGetsTheHelp(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) { a.SlashCommand = "/agent" })

	sendEvent(t, srv, dmThreadEvent("U1", "hello", "200.000", "100.000"))
	require.Eventually(t, func() bool { return len(helpReplies(fake, "100.000")) == 1 },
		flowWait, 20*time.Millisecond, "the help is posted in the chat")
	require.Equal(t, "D1", helpReplies(fake, "100.000")[0].params["channel"])
	require.Contains(t, fmt.Sprint(helpReplies(fake, "100.000")[0].params["text"]), "/agent [question]")
	require.Zero(t, gw.dispatchCount(), "nothing is dispatched")
}

// A command word claims a thread for its person without opening a
// conversation: `@bot usage` on a channel message writes the thread's initiator
// and no agent. A plain reply there is nobody's to answer, as in any thread the
// agent is not in — not the help, which would answer everyone in the thread for
// the row's lifetime — while a mention still gets the help.
func TestNoConversation_ClaimedThreadStaysSilentToPlainReplies(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "<@UBOT> usage", "700.000", ""))
	fake.waitForPath(t, "chat.postMessage", 1)
	row, ok, err := gw.rec().ThreadRecord(context.Background(), "slack", "C1", "700.000")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "U1", row.Initiator, "the command claimed the thread")
	require.Empty(t, row.AgentRef, "without opening a conversation")

	sendEvent(t, srv, threadReply("U1", "so what now?", "700.001", "700.000"))
	sendEvent(t, srv, threadReply("U2", "no idea", "700.002", "700.000"))
	time.Sleep(150 * time.Millisecond)
	require.Len(t, fake.pathCalls("chat.postMessage"), 1, "a plain reply in a thread without a conversation gets no answer")
	require.Empty(t, fake.pathCalls("chat.postEphemeral"))
	require.Zero(t, gw.dispatchCount())

	sendEvent(t, srv, mention("U2", "<@UBOT> can you look?", "700.003", "700.000"))
	require.Eventually(t, func() bool { return len(helpReplies(fake, "700.000")) == 1 },
		flowWait, 20*time.Millisecond, "a mention there gets the help")
	require.Zero(t, gw.dispatchCount())
}

// A thread row the store cannot read is not "no conversation": the message is
// not handled, its author is told, and no help goes out.
func TestNoConversation_StoreOutageIsNotNoConversation(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{recordsErr: errors.New("valkey down")}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "hello", "500.000"))
	require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) == 1 },
		flowWait, 20*time.Millisecond, "the author is told the thread state could not be read")
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "The thread state could not be read")
	require.Empty(t, fake.pathCalls("chat.postMessage"), "no help: the store, not the conversation, is what is missing")
	require.Zero(t, gw.dispatchCount())
}
