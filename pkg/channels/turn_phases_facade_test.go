package channels_test

import (
	"testing"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// The facade marks the turn's timeline it finds on the context: the instance
// create of a first turn as a span, the first A2A event, the task's terminal
// state and the stream's end as marks, and the task id once the controller
// names it. A follow-up on the same thread creates no instance and records no
// create span.
func TestFacade_SendCompletionViaA2A_MarksTurnPhases(t *testing.T) {
	agent := newFakeAgent(
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart("pong")),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	)
	f, _ := newA2AFacade(agent)

	ctx, timer := channels.BeginTurn(t.Context(), "slack", time.Now())
	ch, err := f.SendCompletion(ctx, channels.InstanceRef{}, slackMsg("hi"))
	require.NoError(t, err)
	drain(t, ch)

	phases := timer.Phases()
	require.Contains(t, phases, channels.PhaseCreateInstance, "the first turn creates the instance")
	require.Contains(t, phases, channels.PhaseFirstEvent)
	require.Contains(t, phases, channels.PhaseTaskDone)
	require.Contains(t, phases, channels.PhaseStreamEnd)
	require.LessOrEqual(t, phases[channels.PhaseFirstEvent], phases[channels.PhaseTaskDone])
	require.LessOrEqual(t, phases[channels.PhaseTaskDone], phases[channels.PhaseStreamEnd])
	require.Equal(t, string(taskInfo.TaskID), timer.TaskID())

	ctx2, timer2 := channels.BeginTurn(t.Context(), "slack", time.Now())
	ch, err = f.SendCompletion(ctx2, channels.InstanceRef{}, slackMsg("again"))
	require.NoError(t, err)
	drain(t, ch)
	require.NotContains(t, timer2.Phases(), channels.PhaseCreateInstance, "a follow-up reuses the thread's instance")
	require.Contains(t, timer2.Phases(), channels.PhaseFirstEvent)

	// A context without a timer is served the same way.
	ch, err = f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("plain"))
	require.NoError(t, err)
	drain(t, ch)
}
