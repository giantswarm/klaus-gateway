package slack_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// agingRecorder is a thread recorder a test can age: one clock drives both the
// conversation's lifetime (the facade's) and the row's own expiry (the memory
// store's), so advancing it past the lifetime closes the conversation and
// advancing it past twice the lifetime drops the row, exactly as in an
// installation.
func agingRecorder(t *testing.T) (*slackadapter.MemoryRecorder, func(time.Duration)) {
	t.Helper()
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	mem := memory.New()
	mem.SetNowFunc(clock)
	t.Cleanup(func() { _ = mem.Close() })
	rec := &channels.Facade{Routes: mem, ThreadTTL: channels.DefaultThreadTTL}
	rec.SetNowFunc(clock)
	return rec, func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
}

// threadReply is a plain channel message in a thread: no mention, so it only
// reaches the agent while the thread's conversation is alive.
func threadReply(user, text, ts, threadTS string) string {
	return fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel_type":"channel","user":%q,"text":%q,"channel":"C1","ts":%q,"thread_ts":%q}}`, user, text, ts, threadTS)
}

// A reply without a mention in a thread whose conversation ended after the
// lifetime gets one private line telling its author so, instead of the silence
// that reads as an outage. Nothing is dispatched, and every further reply is
// told again — the notice is ephemeral, so it costs no write and no noise.
func TestClosedThread_UnmentionedReplyGetsTheNotice(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	rec, advance := agingRecorder(t)
	gw.records = rec
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "800.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "800.000")
	before := len(fake.pathCalls("chat.postEphemeral"))

	advance(channels.DefaultThreadTTL + time.Hour)

	sendEvent(t, srv, threadReply("U1", "and now?", "800.001", "800.000"))
	require.Eventually(t, func() bool {
		return len(fake.pathCalls("chat.postEphemeral")) == before+1
	}, flowWait, 20*time.Millisecond, "the reply's author is told the conversation ended")
	text := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, text, "This conversation ended after 90 days without messages.")
	require.Contains(t, text, "Mention me to start a new one.")
	require.Equal(t, 1, gw.resolveCount(), "nothing is dispatched into a conversation that ended")

	sendEvent(t, srv, threadReply("U2", "anyone?", "800.002", "800.000"))
	require.Eventually(t, func() bool {
		return len(fake.pathCalls("chat.postEphemeral")) == before+2
	}, flowWait, 20*time.Millisecond, "every reply in the ended conversation is told, each author privately")
	require.Equal(t, 1, gw.resolveCount())
	require.Empty(t, fake.pathCalls("conversations.replies"), "the state comes from the row, never from Slack history")
}

// A mention in a thread whose conversation ended starts the thread over: the
// mentioner is the new initiator, nothing of the earlier conversation's agent
// binding is kept, and the colleague the old initiator had allowed has to be
// allowed again.
func TestClosedThread_MentionStartsTheThreadOver(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, resolved := capturingGateway()
	rec, advance := agingRecorder(t)
	gw.records = rec
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
	ctx := context.Background()

	sendEvent(t, srv, mention("U1", "look at the nodes", "810.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "810.000")

	// The conversation as it stood: bound to an agent and its instance, with
	// a colleague the initiator allowed in.
	require.NoError(t, rec.UpdateThreadRecord(ctx, "slack", "C1", "810.000", func(e *store.Entry, _ bool) bool {
		e.AgentRef, e.AgentInstanceID = "issue-agent", "inst-old"
		e.Granted = append(e.Granted, "U2")
		return true
	}))

	advance(channels.DefaultThreadTTL + time.Hour)

	sendEvent(t, srv, mention("U3", "picking this back up", "810.001", "810.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "810.000")

	row, ok, err := rec.ThreadRecord(ctx, "slack", "C1", "810.000")
	require.NoError(t, err)
	require.True(t, ok, "the mention re-opened the thread")
	require.Equal(t, "U3", row.Initiator, "the mentioner is the new initiator")
	require.Empty(t, row.Granted, "no grant of the ended conversation is carried over")
	require.Empty(t, row.AgentInstanceID, "the binding is cleared, so the turn binds a fresh instance")
	require.Equal(t, "test-agent", resolved()[1].AgentRef, "and the turn runs on the default agent, not the ended conversation's")

	// The colleague the old initiator had allowed is a newcomer again.
	sendEvent(t, srv, threadReply("U2", "on it", "810.002", "810.000"))
	require.Eventually(t, func() bool {
		return len(fake.pathCalls("chat.postEphemeral")) > 0
	}, flowWait, 20*time.Millisecond)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "waiting for the thread owner",
		"the colleague has to be allowed in again")
	require.Equal(t, 2, gw.resolveCount(), "and their message does not reach the agent meanwhile")
}

// A reply in a thread the store has no row for stays silent, as before: the
// bot may simply never have been in it. That covers the thread it never saw
// and the one whose row the store dropped at twice the lifetime.
func TestClosedThread_ReplyWithoutARowStaysSilent(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	rec, advance := agingRecorder(t)
	gw.records = rec
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, threadReply("U1", "lunch anyone?", "820.001", "820.000"))
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, gw.resolveCount())
	require.Empty(t, fake.pathCalls("chat.postEphemeral"), "a thread the bot was never in is none of its business")

	sendEvent(t, srv, mention("U1", "have a look", "830.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "830.000")
	before := len(fake.pathCalls("chat.postEphemeral"))

	advance(2*channels.DefaultThreadTTL + time.Hour)

	sendEvent(t, srv, threadReply("U1", "still there?", "830.001", "830.000"))
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, 1, gw.resolveCount())
	require.Len(t, fake.pathCalls("chat.postEphemeral"), before,
		"the row is gone, so there is nothing left to tell its author")
}

// A mention in a thread reaches the gateway twice: as message.channels and as
// app_mention. In a thread whose conversation ended, the message twin lands on
// the inactive-thread gate, where the notice would tell its author to mention
// the bot — which is what they just did. Which twin arrives first is a race,
// so the gate recognises the mention itself: the message twin says nothing and
// only the app_mention twin acts.
func TestClosedThread_MentionTwinIsNotToldTheConversationEnded(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	rec, advance := agingRecorder(t)
	gw.records = rec
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)

	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "840.000", ""))
	require.Eventually(t, func() bool { return gw.resolveCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "840.000")
	before := len(fake.pathCalls("chat.postEphemeral"))

	advance(channels.DefaultThreadTTL + time.Hour)

	// The message twin first, on its own: it must post nothing and must not
	// claim the message's dedup slot.
	sendEvent(t, srv, threadReply("U1", "<@UBOT> picking this back up", "840.001", "840.000"))
	time.Sleep(150 * time.Millisecond)
	require.Len(t, fake.pathCalls("chat.postEphemeral"), before,
		"the mention's message twin must not say the conversation ended")
	require.Equal(t, 1, gw.resolveCount(), "and it does not dispatch either")

	// Then the app_mention twin of the same message, which starts over.
	sendEvent(t, srv, mention("U1", "<@UBOT> picking this back up", "840.001", "840.000"))
	require.Eventually(t, func() bool { return gw.resolveCount() == 2 }, flowWait, 20*time.Millisecond,
		"the app_mention twin starts the conversation over")
	waitThreadIdle(t, a, "840.000")
	require.Equal(t, 2, gw.resolveCount(), "the agent answers once")
	require.Len(t, fake.pathCalls("chat.postEphemeral"), before,
		"and its author is never told the conversation ended")
}
