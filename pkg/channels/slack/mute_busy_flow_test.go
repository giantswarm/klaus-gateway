package slack_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
)

// mute in a thread where the agent is busy: a running turn is stopped, an
// open approval card is rejected, and an open question refuses the mute.

const (
	stoppedMutedNote  = "Stopped and muted. I won't reply here until someone mentions me."
	questionOpenNote  = "The agent asked a question here. Answer it, or mention the agent, before you mute."
	notPermittedShort = "only the people the thread owner allowed can instruct the agent"
)

func approvalDelta() channels.OutboundDelta {
	return channels.OutboundDelta{Kind: channels.DeltaPrompt, TaskID: "task-1",
		Prompt: &channels.HitlPrompt{ToolName: "kubectl_delete", StatusText: "Delete the pod?"}}
}

func questionDelta() channels.OutboundDelta {
	return channels.OutboundDelta{Kind: channels.DeltaPrompt, TaskID: "task-1",
		Prompt: &channels.HitlPrompt{
			ToolName:  channels.AskUserToolName,
			Questions: []channels.HitlQuestion{{Question: "Which cluster?", Choices: []string{"gazelle", "graveler"}}},
		}}
}

// openThread opens a conversation in thread 500.000 with U1 as its initiator.
func openThread(t *testing.T, gw *stubGateway, fake *fakeSlackAPI) (*slackadapter.Adapter, *httptest.Server) {
	t.Helper()
	a, srv := newEventsAdapter(t, gw, fake.server(t).URL, channelMode)
	sendEvent(t, srv, mention("U1", "why is the cluster unhappy?", "500.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	return a, srv
}

// mute while the agent answers stops the answer the way stop does and mutes
// the thread, with one note that says both.
func TestMuteBusy_RunningTurnIsStopped(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	defer close(hold)
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "thinking"}}, hold: hold}
	a, srv := openThread(t, gw, fake)

	sendEvent(t, srv, threadReply("U1", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), stoppedMutedNote)
	}, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	require.Len(t, gw.sendCauseList(), 1)
	require.ErrorIs(t, gw.sendCauseList()[0], context.Canceled, "the running turn's send was cancelled")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"))
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), mutedNote, "one note, not two")
	require.Equal(t, 1, gw.dispatchCount(), "the word is consumed")
}

// Someone who may not instruct the agent gets the not-permitted note: the
// running turn goes on and the thread is not muted.
func TestMuteBusy_NotPermittedStopsNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	hold := make(chan struct{})
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "thinking"}}, hold: hold}
	a, srv := openThread(t, gw, fake)

	sendEvent(t, srv, threadReply("U2", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), notPermittedShort)
	}, flowWait, 20*time.Millisecond)
	require.Empty(t, gw.sendCauseList(), "the turn is not stopped")
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"))

	close(hold)
	waitThreadIdle(t, a, "500.000")
}

// mute beside an open approval card rejects it the way a typed deny word
// does — a plain Reject resumes the paused task — and the thread stays muted:
// the resume runs on the mute's own message, which does not end the mute.
func TestMuteBusy_ApprovalIsRejected(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, dispatched := capturingGateway()
	gw.sendQueue = [][]channels.OutboundDelta{{approvalDelta()}, {{Content: "left it"}, {Done: true}}}
	a, srv := openThread(t, gw, fake)
	waitThreadIdle(t, a, "500.000")

	sendEvent(t, srv, threadReply("U1", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	resume := dispatched()[1]
	require.Equal(t, "task-1", resume.TaskID)
	require.Equal(t, &channels.HitlDecision{Type: channels.DecisionReject}, resume.Decision, "a plain rejection, no reason")
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), mutedNote)
	require.NotEmpty(t, fake.pathCalls("chat.update"), "the card shows the rejection")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"), "the resume does not end the mute")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote)
}

// mute beside an open question refuses privately, is not sent as the answer,
// and leaves the thread unmuted with the question still open.
func TestMuteBusy_OpenQuestionRefuses(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, dispatched := capturingGateway()
	gw.sendQueue = [][]channels.OutboundDelta{{questionDelta()}, {{Content: "graveler it is"}, {Done: true}}}
	a, srv := openThread(t, gw, fake)
	waitThreadIdle(t, a, "500.000")

	sendEvent(t, srv, threadReply("U1", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), questionOpenNote)
	}, flowWait, 20*time.Millisecond)
	require.Never(t, func() bool { return gw.dispatchCount() > 1 }, 500*time.Millisecond, 20*time.Millisecond,
		"the word is not the question's answer")
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"))

	sendEvent(t, srv, threadReply("U1", "graveler", "500.002", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond,
		"the question is still open and takes its answer")
	require.Equal(t, [][]string{{"graveler"}}, dispatched()[1].Decision.AskUserAnswers)
}

// A question without a structured prompt — the typed reply is its answer —
// refuses the mute too.
func TestMuteBusy_UnstructuredQuestionRefuses(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	gw.sendQueue = [][]channels.OutboundDelta{{{Kind: channels.DeltaPrompt, TaskID: "task-1"}}}
	a, srv := openThread(t, gw, fake)
	waitThreadIdle(t, a, "500.000")

	sendEvent(t, srv, threadReply("U1", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), questionOpenNote)
	}, flowWait, 20*time.Millisecond)
	require.Never(t, func() bool { return gw.dispatchCount() > 1 }, 500*time.Millisecond, 20*time.Millisecond)
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"))
}

// Someone who may not instruct the agent cannot reject the card by muting.
func TestMuteBusy_NotPermittedRejectsNothing(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	gw.sendQueue = [][]channels.OutboundDelta{{approvalDelta()}}
	a, srv := openThread(t, gw, fake)
	waitThreadIdle(t, a, "500.000")

	sendEvent(t, srv, threadReply("U2", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), notPermittedShort)
	}, flowWait, 20*time.Millisecond)
	require.Never(t, func() bool { return gw.dispatchCount() > 1 }, 500*time.Millisecond, 20*time.Millisecond,
		"the card is not rejected")
	require.Empty(t, fake.pathCalls("chat.update"))
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"))
}

// muteRejectsIntoPrompt mutes thread 500.000 beside an approval card whose
// rejected task's resumed turn sends then.
func muteRejectsIntoPrompt(t *testing.T, then []channels.OutboundDelta) (*fakeSlackAPI, *stubGateway, func() []channels.InboundMessage, *slackadapter.Adapter, *httptest.Server) {
	t.Helper()
	fake := newFakeSlackAPI()
	gw, dispatched := capturingGateway()
	gw.sendQueue = [][]channels.OutboundDelta{{approvalDelta()}, then}
	a, srv := openThread(t, gw, fake)
	waitThreadIdle(t, a, "500.000")
	sendEvent(t, srv, threadReply("U1", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"))
	return fake, gw, dispatched, a, srv
}

// The rejected task's resumed turn can ask a question in the muted thread. A
// repeat of mute there says the thread is muted, which it is.
func TestMuteBusy_AlreadyMutedBesideAQuestion(t *testing.T) {
	fake, gw, _, _, srv := muteRejectsIntoPrompt(t, []channels.OutboundDelta{questionDelta()})

	sendEvent(t, srv, threadReply("U1", "mute", "500.002", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "Already muted.")
	}, flowWait, 20*time.Millisecond)
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), questionOpenNote)
	require.Never(t, func() bool { return gw.dispatchCount() > 2 }, 500*time.Millisecond, 20*time.Millisecond)
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"))
}

// The rejected task's resumed turn can ask for another approval. A repeat of
// mute rejects that card too, and the thread stays muted from the first mute.
func TestMuteBusy_AlreadyMutedRejectsAnotherCard(t *testing.T) {
	second := channels.OutboundDelta{Kind: channels.DeltaPrompt, TaskID: "task-2",
		Prompt: &channels.HitlPrompt{ToolName: "kubectl_scale"}}
	fake, gw, dispatched, a, srv := muteRejectsIntoPrompt(t, []channels.OutboundDelta{second})

	sendEvent(t, srv, threadReply("U1", "mute", "500.002", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 3 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	require.Equal(t, "task-2", dispatched()[2].TaskID)
	require.Equal(t, &channels.HitlDecision{Type: channels.DecisionReject}, dispatched()[2].Decision)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "Already muted.")
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"), "the word does not end the mute")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote)
}

// A repeat of mute while the rejected task's resumed turn runs stops it.
func TestMuteBusy_AlreadyMutedStopsTheResumedTurn(t *testing.T) {
	fake := newFakeSlackAPI()
	gw, _ := capturingGateway()
	gw.sendQueue = [][]channels.OutboundDelta{{approvalDelta()}, {{Content: "on it"}}}
	a, srv := openThread(t, gw, fake)
	waitThreadIdle(t, a, "500.000")
	hold := make(chan struct{})
	defer close(hold)
	gw.mu.Lock()
	gw.hold = hold
	gw.mu.Unlock()

	sendEvent(t, srv, threadReply("U1", "mute", "500.001", "500.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond,
		"the resumed turn runs and holds the thread")
	sendEvent(t, srv, threadReply("U1", "mute", "500.002", "500.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postMessage")), stoppedMutedNote)
	}, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")

	require.Len(t, gw.sendCauseList(), 1)
	require.ErrorIs(t, gw.sendCauseList()[0], context.Canceled)
	require.Equal(t, "500.001", mutedAt(t, gw.rec(), "500.000"))
}

// A mention whose upload carries the caption "mute" is a message for the
// agent, not the command, so it ends the mute like any other mention.
func TestMuteBusy_CaptionedMentionEndsTheMute(t *testing.T) {
	fake, gw, a, srv, _ := startMutedThread(t)

	sendEvent(t, srv, `{"type":"event_callback","event":{"type":"message","subtype":"file_share","channel_type":"channel","user":"U1","text":"<@UBOT> mute","channel":"C1","ts":"500.007","thread_ts":"500.000","files":[{"name":"graph.png","mimetype":"image/png","url_private":"https://files.slack.com/f.png","size":10}]}}`)
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "500.000")
	require.Empty(t, mutedAt(t, gw.rec(), "500.000"))
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), unmutedNote)
}

// restartWithPrompt pauses thread 600.000 on prompt in one adapter, stops it,
// and starts a second adapter over the same store with the prompt restored.
func restartWithPrompt(t *testing.T, prompt channels.OutboundDelta) (*fakeSlackAPI, *stubGateway, func() []channels.InboundMessage, *slackadapter.Adapter, *httptest.Server) {
	t.Helper()
	fake := newFakeSlackAPI()
	api := fake.server(t)
	shared := slackadapter.NewMemoryRecorder()

	gw1 := &stubGateway{records: shared, sendQueue: [][]channels.OutboundDelta{{prompt}}}
	a1, srv1 := newEventsAdapter(t, gw1, api.URL, channelMode)
	sendEvent(t, srv1, mention("U1", "clean up", "600.000", ""))
	require.Eventually(t, func() bool { return heldIn(t, shared, "C1", "600.000") }, flowWait, 20*time.Millisecond,
		"the paused prompt is in the thread's row")
	waitThreadIdle(t, a1, "600.000")
	require.NoError(t, a1.Stop(context.Background()))

	gw2, dispatched := captureDispatch(shared)
	a2, srv2 := newEventsAdapter(t, gw2, api.URL, channelMode)
	a2.RecoverTurns()
	a2.WaitHeldRestored()
	return fake, gw2, dispatched, a2, srv2
}

// An approval card restored after a restart is rejected by mute the same way.
func TestMuteBusy_RestoredApprovalIsRejected(t *testing.T) {
	fake, gw, dispatched, a, srv := restartWithPrompt(t, approvalDelta())

	sendEvent(t, srv, threadReply("U1", "mute", "600.001", "600.000"))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 }, flowWait, 20*time.Millisecond)
	waitThreadIdle(t, a, "600.000")

	require.Equal(t, "task-1", dispatched()[0].TaskID, "the task paused before the restart is resumed")
	require.Equal(t, &channels.HitlDecision{Type: channels.DecisionReject}, dispatched()[0].Decision)
	require.Contains(t, allText(fake.pathCalls("chat.postMessage")), mutedNote)
	require.Equal(t, "600.001", mutedAt(t, gw.rec(), "600.000"))
}

// A question restored after a restart makes mute refuse the same way.
func TestMuteBusy_RestoredQuestionRefuses(t *testing.T) {
	fake, gw, _, _, srv := restartWithPrompt(t, questionDelta())

	sendEvent(t, srv, threadReply("U1", "mute", "600.001", "600.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), questionOpenNote)
	}, flowWait, 20*time.Millisecond)
	require.Never(t, func() bool { return gw.dispatchCount() > 0 }, 500*time.Millisecond, 20*time.Millisecond)
	require.Empty(t, mutedAt(t, gw.rec(), "600.000"))
}
