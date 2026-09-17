package slack_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// The gateway's shutdown is not a /stop: in reactions mode, where a stop
// leaves no note, the thread is told that the agent keeps working and where
// the answer goes, the working reaction is cleared, and the facade sees the
// shutdown cause on the turn context so it leaves the task running.
func TestShutdown_PostsRestartNoticeAndLeavesTheTurnRunning(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{hold: hold, resumes: &stubResumes{durable: true}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "long task", "555.000"))
	fake.waitForPath(t, "reactions.add", 1)
	waitTurnStreaming(t, fake)

	require.NoError(t, a.Stop(context.Background()))

	posted := allBlockText(fake.pathCalls("chat.postMessage"))
	require.Contains(t, posted, "I was restarted while **test-agent** was working", "the thread is told about the restart")
	require.Contains(t, posted, "I post it here when it is done", "a durable store lets the notice promise the delivery")
	require.NotContains(t, posted, "(stopped)", "a restart is not rendered as a stop")
	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.remove"), "working reaction cleared")
	require.Equal(t, []string{"eyes"}, fake.reactionNames("reactions.add"), "no failed reaction for a restart")
	require.Equal(t, "555.000", fake.pathCalls("chat.postMessage")[len(fake.pathCalls("chat.postMessage"))-1].params["thread_ts"], "the notice lands in the thread")

	causes := gw.sendCauseList()
	require.Len(t, causes, 1)
	require.ErrorIs(t, causes[0], channels.ErrShutdown, "the turn context names the shutdown as its cause")
}

// Without a routing store that outlives the process the record of the turn
// dies with it, so the notice must not promise a delivery it cannot make.
func TestShutdown_NoticeWithoutDurableStoreMakesNoPromise(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{hold: hold}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "long task", "555.000"))
	fake.waitForPath(t, "reactions.add", 1)
	waitTurnStreaming(t, fake)

	require.NoError(t, a.Stop(context.Background()))

	posted := allBlockText(fake.pathCalls("chat.postMessage"))
	require.Contains(t, posted, "I was restarted while **test-agent** was working")
	require.Contains(t, posted, "cannot bring it into this thread")
	require.NotContains(t, posted, "I post it here when it is done")
}

// In text-progress mode the restart notice replaces the "thinking" placeholder
// so it does not linger, just as the stop and failure notes do.
func TestShutdown_TextModeReplacesThePlaceholder(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{hold: hold, resumes: &stubResumes{durable: true}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)
	a.ProgressMode = "text"

	sendEvent(t, srv, dmEvent("U1", "long task", "100.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), "_thinking")
	}, flowWait, 20*time.Millisecond, "text placeholder posted")

	require.NoError(t, a.Stop(context.Background()))

	require.Contains(t, allBlockText(fake.pathCalls("chat.update")), "I was restarted while", "the placeholder is replaced by the notice")
}

// A /stop stays a plain cancellation: the facade sees no shutdown cause and
// cancels the task at the controller (the facade's own test covers the cancel).
func TestStop_IsAPlainCancellationNotAShutdown(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{hold: hold, resumes: &stubResumes{durable: true}}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "long task", "555.000"))
	fake.waitForPath(t, "reactions.add", 1)
	waitTurnStreaming(t, fake)

	sendEvent(t, srv, dmThreadEvent("U1", "/stop", "556.000", "555.000"))
	fake.waitForPath(t, "reactions.remove", 1)

	require.Eventually(t, func() bool { return len(gw.sendCauseList()) == 1 }, flowWait, 20*time.Millisecond)
	cause := gw.sendCauseList()[0]
	require.ErrorIs(t, cause, context.Canceled)
	require.False(t, errors.Is(cause, channels.ErrShutdown), "a /stop must not read as a shutdown")
	require.NotContains(t, allBlockText(fake.pathCalls("chat.postMessage")), "I was restarted", "no restart notice for a /stop")
}

// leftoverTurn is a turn a previous process left running on DM thread 700.000
// for user U1, as the routing store hands it back.
func leftoverTurn(taskID string) channels.InFlightTurn {
	return channels.InFlightTurn{
		Msg: channels.InboundMessage{
			Channel: "slack", ChannelID: "D1", ThreadID: "700.000", AgentRef: "test-agent",
			Resume: map[string]string{"slack_user": "U1", "message_ts": "700.000"},
		},
		TaskID: taskID,
	}
}

// At start the adapter resubscribes to the turns the previous process left
// running and delivers their results into their threads, with the working
// reaction back on the original message while it streams, under the recorded
// user's identity.
func TestRecoverTurns_DeliversTheAnswerIntoTheThread(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{resumes: &stubResumes{
		durable: true,
		turns:   []channels.InFlightTurn{leftoverTurn("task-9")},
		deltas:  map[string][]channels.OutboundDelta{"task-9": {{Content: "recovered answer"}, {Done: true}}},
	}}
	a, _ := newEventsAdapter(t, gw, fake.server(t).URL)

	a.RecoverTurns()

	require.Eventually(t, func() bool {
		return strings.Contains(allBlockText(fake.pathCalls("chat.postMessage")), "recovered answer")
	}, 5*time.Second, 20*time.Millisecond, "the finished answer is posted into the thread")
	fake.waitForPath(t, "reactions.add", 2)

	posts := fake.pathCalls("chat.postMessage")
	require.Equal(t, "700.000", posts[len(posts)-1].params["thread_ts"], "the answer lands in the original thread")
	require.Equal(t, "700.000", fake.pathCalls("reactions.add")[0].params["timestamp"], "the working reaction returns to the triggering message")
	require.Contains(t, fake.reactionNames("reactions.add"), "white_check_mark", "the resumed turn completes like any other")

	gw.mu.Lock()
	defer gw.mu.Unlock()
	require.Equal(t, []string{"task-9"}, gw.resumes.resumedTasks)
	require.Equal(t, "U1", gw.resumes.resumed[0].Subject, "the delivery runs as the recorded user")
	require.Equal(t, "700.000", gw.resumes.resumed[0].ThreadID)
}

// A reply into a thread whose left-running turn the start-up recovery did not
// deliver finds the record and delivers the answer first, then runs as usual,
// so the thread reads in order.
func TestReply_DeliversTheLeftoverTurnBeforeItself(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{
		deltas: []channels.OutboundDelta{{Content: "reply answer"}, {Done: true}},
		resumes: &stubResumes{
			durable: true,
			turns:   []channels.InFlightTurn{leftoverTurn("task-9")},
			deltas:  map[string][]channels.OutboundDelta{"task-9": {{Content: "recovered answer"}, {Done: true}}},
		},
	}
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmThreadEvent("U1", "and then?", "701.000", "700.000"))

	require.Eventually(t, func() bool {
		return strings.Contains(allBlockText(fake.pathCalls("chat.postMessage")), "reply answer")
	}, 5*time.Second, 20*time.Millisecond, "the reply is answered")
	posted := allBlockText(fake.pathCalls("chat.postMessage"))
	recovered, reply := strings.Index(posted, "recovered answer"), strings.Index(posted, "reply answer")
	require.GreaterOrEqual(t, recovered, 0, "the leftover turn's answer is delivered")
	require.Less(t, recovered, reply, "the leftover answer lands before the reply's own")

	gw.mu.Lock()
	defer gw.mu.Unlock()
	require.Equal(t, []string{"task-9"}, gw.resumes.resumedTasks)
	require.Empty(t, gw.resumes.turns, "the record is consumed by the delivery")
}

// A left-running turn whose task the controller no longer has is closed out
// with a note instead of a promise nobody can keep.
func TestRecoverTurns_GoneTaskPostsANote(t *testing.T) {
	fake := newFakeSlackAPI()
	gw := &stubGateway{resumes: &stubResumes{
		durable:   true,
		turns:     []channels.InFlightTurn{leftoverTurn("task-9")},
		resumeErr: errTaskGone,
	}}
	a, _ := newEventsAdapter(t, gw, fake.server(t).URL)

	a.RecoverTurns()

	require.Eventually(t, func() bool {
		return strings.Contains(allBlockText(fake.pathCalls("chat.postMessage")), "couldn't recover the result")
	}, 5*time.Second, 20*time.Millisecond, "the thread is told the result is gone")
}

// Text the writer still holds when the shutdown hits lands before the notice:
// the next process continues from where the stream was cut, so nothing may
// stay behind in this one's buffer.
func TestShutdown_FlushesBufferedTextBeforeTheNotice(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{hold: hold, deltas: []channels.OutboundDelta{{Content: "counting: 1, 2, 3"}}, resumes: &stubResumes{durable: true}}
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL)

	sendEvent(t, srv, dmEvent("U1", "count", "555.000"))
	fake.waitForPath(t, "reactions.add", 1)
	waitTurnStreaming(t, fake)
	// The turn is resolved before its first delta is read: wait for the text to
	// have reached the writer, or the shutdown below flushes an empty buffer.
	require.Eventually(t, func() bool { return len(gw.sendCauseList()) == 0 && gw.deliveredDeltas() == 1 }, flowWait, 20*time.Millisecond)

	require.NoError(t, a.Stop(context.Background()))

	posts := fake.pathCalls("chat.postMessage")
	texts := allBlockText(posts)
	content, notice := strings.Index(texts, "counting: 1, 2, 3"), strings.Index(texts, "I was restarted")
	require.GreaterOrEqual(t, content, 0, "the buffered text is delivered")
	require.Less(t, content, notice, "the text lands before the notice")
}
