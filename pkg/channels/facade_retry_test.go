package channels_test

import (
	"errors"
	"testing"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// toolSetFailure is the runtime's report of a turn whose MCP tool set could
// not be initialized, as the Go ADK words it.
const toolSetFailure = `failed to extract tools from the tool set "mcp_tool_set": failed to list MCP tools: failed to init MCP session: calling "initialize": sending "initialize": Post "http://muster.agent-platform.svc:8090/mcp": read tcp 10.0.0.7:41234->10.0.0.9:8090: read: connection reset by peer`

func submitted() a2apkg.Event {
	return &a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}}
}

func failedWith(text string) a2apkg.Event {
	return a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateFailed, a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart(text)))
}

func answered(text string) []a2apkg.Event {
	return []a2apkg.Event{
		submitted(),
		a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart(text)),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	}
}

// sendTimed sends msg with a turn timeline on the context and returns the
// turn's deltas and its timer.
func sendTimed(t *testing.T, f *channels.Facade, msg channels.InboundMessage) ([]channels.OutboundDelta, *channels.TurnTimer, error) {
	t.Helper()
	timer := channels.NewTurnTimer(time.Time{})
	ch, err := f.SendCompletion(channels.WithTurnTimer(t.Context(), timer), msg)
	if err != nil {
		return nil, timer, err
	}
	return drain(t, ch), timer, nil
}

// answerOf is the text and the terminal state of a turn's deltas.
func answerOf(deltas []channels.OutboundDelta) (text string, done bool, err error) {
	for _, d := range deltas {
		text += d.Content
		done = done || d.Done
		if d.Err != nil {
			err = d.Err
		}
	}
	return text, done, err
}

// A turn whose tool set could not be set up is sent once more on the same
// instance before anything was shown, and the channel sees only the attempt
// that answered.
func TestFacade_ToolSetFailureIsRetriedOnce(t *testing.T) {
	agent := newFakeAgent()
	agent.attempts = []streamAttempt{
		{events: []a2apkg.Event{submitted(), a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, nil), failedWith(toolSetFailure)}},
		{events: answered("7 nodes, all Ready.")},
	}
	f, _ := newA2AFacade(agent)

	deltas, timer, err := sendTimed(t, f, slackMsg("how many nodes?"))
	require.NoError(t, err)

	text, done, turnErr := answerOf(deltas)
	require.NoError(t, turnErr, "the failed attempt never reaches the channel")
	require.True(t, done)
	require.Equal(t, "7 nodes, all Ready.", text)
	require.Len(t, agent.streamed, 2)
	require.Equal(t, agent.streamedOn[0], agent.streamedOn[1], "the retry runs on the thread's instance: a new one would start the conversation over")
	require.Equal(t, 1, agent.created, "the instance is created once")
	require.Equal(t, 1, timer.Retries())
}

// A second tool-set failure is the turn's failure: there is one retry.
func TestFacade_ToolSetFailureIsRetriedOnlyOnce(t *testing.T) {
	agent := newFakeAgent()
	agent.attempts = []streamAttempt{
		{events: []a2apkg.Event{submitted(), failedWith(toolSetFailure)}},
		{events: []a2apkg.Event{submitted(), failedWith(toolSetFailure)}},
		{events: answered("never sent")},
	}
	f, _ := newA2AFacade(agent)

	deltas, timer, err := sendTimed(t, f, slackMsg("how many nodes?"))
	require.NoError(t, err)

	_, done, turnErr := answerOf(deltas)
	require.False(t, done)
	require.ErrorContains(t, turnErr, "failed to init MCP session")
	require.Equal(t, channels.FailureTools, channels.ClassifyFailure(turnErr))
	require.Len(t, agent.streamed, 2)
	require.Equal(t, 1, timer.Retries())
}

// What the turn already showed cannot be taken back, so a failure after it
// is the turn's failure, and so is a failure a second attempt cannot get
// past.
func TestFacade_NoRetryAfterOutputOrOnAnUnretryableFailure(t *testing.T) {
	for name, events := range map[string][]a2apkg.Event{
		"text shown": {submitted(), a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart("Let me look.")), failedWith(toolSetFailure)},
		"model":      {submitted(), failedWith(`anthropic API error: POST "https://api.anthropic.com/v1/messages": 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)},
		"unknown":    {submitted(), failedWith("something odd happened")},
	} {
		t.Run(name, func(t *testing.T) {
			agent := newFakeAgent()
			agent.attempts = []streamAttempt{{events: events}, {events: answered("never sent")}}
			f, _ := newA2AFacade(agent)

			deltas, timer, err := sendTimed(t, f, slackMsg("how many nodes?"))
			require.NoError(t, err)

			_, _, turnErr := answerOf(deltas)
			require.Error(t, turnErr)
			require.Len(t, agent.streamed, 1)
			require.Zero(t, timer.Retries())
		})
	}
}

// A stream that breaks is not a task that ended: the task may run on at the
// controller, and a second message would run beside it.
func TestFacade_BrokenStreamIsNotRetried(t *testing.T) {
	agent := newFakeAgent(submitted())
	agent.tailErr = errors.New("rpc error: code = Unavailable desc = read: connection reset by peer")
	f, _ := newA2AFacade(agent)

	deltas, timer, err := sendTimed(t, f, slackMsg("hi"))
	require.NoError(t, err)

	_, _, turnErr := answerOf(deltas)
	require.ErrorContains(t, turnErr, "connection reset by peer")
	require.Equal(t, channels.FailurePlatform, channels.ClassifyFailure(turnErr))
	require.Len(t, agent.streamed, 1)
	require.Zero(t, timer.Retries())
}

// A controller that cannot be reached refuses the send itself; the send is
// tried once more.
func TestFacade_UnreachableControllerIsRetriedOnce(t *testing.T) {
	agent := newFakeAgent()
	agent.attempts = []streamAttempt{
		{err: errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing: dial tcp 10.0.0.3:8083: connect: connection refused"`)},
		{events: answered("here you go")},
	}
	f, _ := newA2AFacade(agent)

	deltas, timer, err := sendTimed(t, f, slackMsg("hi"))
	require.NoError(t, err)

	text, done, turnErr := answerOf(deltas)
	require.NoError(t, turnErr)
	require.True(t, done)
	require.Equal(t, "here you go", text)
	require.Len(t, agent.streamed, 2)
	require.Equal(t, 1, timer.Retries())
}

// A decision resumes the paused task; once that failed there is nothing to
// resume, so it is sent once.
func TestFacade_ResumeIsNotRetried(t *testing.T) {
	agent := newFakeAgent()
	agent.attempts = []streamAttempt{
		{events: []a2apkg.Event{failedWith(toolSetFailure)}},
		{events: answered("never sent")},
	}
	f, _ := newA2AFacade(agent)
	msg := slackMsg("yes")
	msg.TaskID = string(taskInfo.TaskID)

	deltas, timer, err := sendTimed(t, f, msg)
	require.NoError(t, err)

	_, _, turnErr := answerOf(deltas)
	require.ErrorContains(t, turnErr, "failed to init MCP session")
	require.Len(t, agent.streamed, 1)
	require.Zero(t, timer.Retries())
}
