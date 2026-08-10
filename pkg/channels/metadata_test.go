package channels

import (
	"testing"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"
)

func dataPart(t *testing.T, kagentType string, data map[string]any) *a2apkg.Part {
	t.Helper()
	p := a2apkg.NewDataPart(data)
	p.Metadata = map[string]any{mdTypeKagent: kagentType}
	return p
}

func usageMeta(prompt, completion, total float64) map[string]any {
	return map[string]any{
		mdUsageKagent: map[string]any{
			usagePromptTokens:     prompt,
			usageCompletionTokens: completion,
			usageTotalTokens:      total,
		},
	}
}

func TestMapA2AEvent_CompletedCarriesUsage(t *testing.T) {
	ev := &a2apkg.TaskStatusUpdateEvent{
		TaskID:   "task-1",
		Metadata: usageMeta(10, 5, 15),
		Status:   a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
	}

	deltas := mapA2AEvent(ev)
	require.Len(t, deltas, 1)
	require.True(t, deltas[0].Done)
	require.NotNil(t, deltas[0].Usage)
	require.Equal(t, TurnUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, *deltas[0].Usage)
}

func TestMapA2AEvent_ArtifactTextAndToolActivity(t *testing.T) {
	call := dataPart(t, mdTypeFunctionCall, map[string]any{
		"name": "kubectl_get",
		"args": map[string]any{"resource": "pods"},
		"id":   "call-1",
	})
	ev := &a2apkg.TaskArtifactUpdateEvent{
		Artifact: &a2apkg.Artifact{
			Parts: a2apkg.ContentParts{a2apkg.NewTextPart("here you go"), call},
		},
	}

	deltas := mapA2AEvent(ev)
	require.Len(t, deltas, 2)
	require.Equal(t, DeltaText, deltas[0].Kind)
	require.Equal(t, "here you go", deltas[0].Content)
	require.Equal(t, DeltaToolActivity, deltas[1].Kind)
	require.Equal(t, "kubectl_get", deltas[1].Content)
	require.NotNil(t, deltas[1].Tool)
	require.Equal(t, ToolCall, deltas[1].Tool.Kind)
	require.Equal(t, "kubectl_get", deltas[1].Tool.Name)
	require.Equal(t, "call-1", deltas[1].Tool.CallID)
	require.Equal(t, "pods", deltas[1].Tool.Args["resource"])
}

func TestMapA2AEvent_InterimToolActivityAndUsage(t *testing.T) {
	resp := dataPart(t, mdTypeFunctionResponse, map[string]any{
		"name":     "kubectl_get",
		"response": map[string]any{"output": "pod/foo"},
		"id":       "call-1",
	})
	ev := &a2apkg.TaskStatusUpdateEvent{
		Metadata: usageMeta(3, 4, 7),
		Status: a2apkg.TaskStatus{
			State:   a2apkg.TaskStateWorking,
			Message: &a2apkg.Message{Parts: a2apkg.ContentParts{resp}},
		},
	}

	deltas := mapA2AEvent(ev)
	require.Len(t, deltas, 2)
	require.Equal(t, DeltaToolActivity, deltas[0].Kind)
	require.Equal(t, ToolResult, deltas[0].Tool.Kind)
	require.Equal(t, "kubectl_get", deltas[0].Tool.Name)
	require.NotNil(t, deltas[1].Usage)
	require.Equal(t, 7, deltas[1].Usage.TotalTokens)
}

func TestMapA2AEvent_ConfirmationPartIsNotToolActivity(t *testing.T) {
	// A long-running function_call is an adk_request_confirmation (HITL), routed
	// via input-required, not surfaced as tool activity.
	confirm := a2apkg.NewDataPart(map[string]any{"name": confirmationToolName})
	confirm.Metadata = map[string]any{mdTypeKagent: mdTypeFunctionCall, mdLongRunningKagent: true}
	ev := &a2apkg.TaskArtifactUpdateEvent{
		Artifact: &a2apkg.Artifact{Parts: a2apkg.ContentParts{confirm}},
	}
	require.Empty(t, mapA2AEvent(ev))
}

// The prose the agent writes before firing its tool calls rides on the same
// working event as the calls, and must reach the channel ahead of them
// (klaus-gateway#197).
func TestMapA2AEvent_NarrationPrecedesToolActivity(t *testing.T) {
	call := func(id string) *a2apkg.Part {
		return dataPart(t, mdTypeFunctionCall, map[string]any{"name": "kubectl_get", "id": id})
	}
	ev := &a2apkg.TaskStatusUpdateEvent{
		Metadata: usageMeta(3, 4, 7),
		Status: a2apkg.TaskStatus{
			State: a2apkg.TaskStateWorking,
			Message: a2apkg.NewMessage(a2apkg.MessageRoleAgent,
				a2apkg.NewTextPart("Let me pull the HelmRelease from both clusters simultaneously."),
				call("call-1"), call("call-2")),
		},
	}

	deltas := mapA2AEvent(ev)
	require.Len(t, deltas, 4)
	require.Equal(t, DeltaNarration, deltas[0].Kind)
	require.Equal(t, "Let me pull the HelmRelease from both clusters simultaneously.", deltas[0].Content)
	require.Equal(t, DeltaToolActivity, deltas[1].Kind)
	require.Equal(t, "call-1", deltas[1].Tool.CallID)
	require.Equal(t, DeltaToolActivity, deltas[2].Kind)
	require.Equal(t, "call-2", deltas[2].Tool.CallID)
	require.NotNil(t, deltas[3].Usage)
}

// kagent mirrors the final answer as a text-only working event and then re-sends
// it as the turn's artifact. Only the artifact is rendered, so the mirror must
// stay silent or the answer would appear twice.
func TestMapA2AEvent_TextOnlyWorkingEventEmitsNoNarration(t *testing.T) {
	ev := &a2apkg.TaskStatusUpdateEvent{
		Status: a2apkg.TaskStatus{
			State:   a2apkg.TaskStateWorking,
			Message: a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("here is the diff")),
		},
	}
	require.Empty(t, mapA2AEvent(ev))
}

// kagent echoes the inbound user message as the submitted event of a new task.
func TestMapA2AEvent_UserEchoEmitsNothing(t *testing.T) {
	ev := &a2apkg.TaskStatusUpdateEvent{
		Status: a2apkg.TaskStatus{
			State:   a2apkg.TaskStateSubmitted,
			Message: a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("compare both clusters")),
		},
	}
	require.Empty(t, mapA2AEvent(ev))
}

// A streaming chunk is repeated in full by the non-partial event that follows it,
// so its text is not narration. Tool activity on such an event is unaffected.
func TestMapA2AEvent_PartialNarrationSkipped(t *testing.T) {
	for _, key := range []string{mdPartialKagent, mdPartialADK} {
		for _, on := range []string{"event", "message"} {
			t.Run(key+"/"+on, func(t *testing.T) {
				call := dataPart(t, mdTypeFunctionCall, map[string]any{"name": "kubectl_get", "id": "call-1"})
				msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("let me look"), call)
				ev := &a2apkg.TaskStatusUpdateEvent{
					Status: a2apkg.TaskStatus{State: a2apkg.TaskStateWorking, Message: msg},
				}
				if on == "event" {
					ev.Metadata = map[string]any{key: true}
				} else {
					msg.Metadata = map[string]any{key: true}
				}

				deltas := mapA2AEvent(ev)
				require.Len(t, deltas, 1)
				require.Equal(t, DeltaToolActivity, deltas[0].Kind)
			})
		}
	}
}

// A message asking for confirmation is rendered by the input-required path, which
// uses the same text as the prompt body; narrating it too would duplicate it.
func TestMapA2AEvent_ConfirmationBesideToolCallEmitsNoNarration(t *testing.T) {
	confirm := a2apkg.NewDataPart(map[string]any{"name": confirmationToolName})
	confirm.Metadata = map[string]any{mdTypeKagent: mdTypeFunctionCall, mdLongRunningKagent: true}
	call := dataPart(t, mdTypeFunctionCall, map[string]any{"name": "kubectl_delete", "id": "call-1"})
	ev := &a2apkg.TaskStatusUpdateEvent{
		Status: a2apkg.TaskStatus{
			State: a2apkg.TaskStateWorking,
			Message: a2apkg.NewMessage(a2apkg.MessageRoleAgent,
				a2apkg.NewTextPart("I need your approval to delete this."), call, confirm),
		},
	}

	deltas := mapA2AEvent(ev)
	require.Len(t, deltas, 1)
	require.Equal(t, DeltaToolActivity, deltas[0].Kind)
}

func TestOutboundDelta_IsZero(t *testing.T) {
	require.True(t, OutboundDelta{}.isZero())
	require.False(t, OutboundDelta{Usage: &TurnUsage{}}.isZero())
	require.False(t, OutboundDelta{Kind: DeltaToolActivity, Content: "x"}.isZero())
	require.False(t, OutboundDelta{Tool: &ToolActivity{Name: "x"}}.isZero())
}

// Partial (streaming) events mirror the usage metadata of the LLM call they
// belong to; counting them would tally one call several times. kagent marks
// them with adk_partial/kagent_partial.
func TestMapA2AEvent_PartialEventUsageSkipped(t *testing.T) {
	for _, key := range []string{mdPartialKagent, mdPartialADK} {
		t.Run(key, func(t *testing.T) {
			meta := usageMeta(3, 4, 7)
			meta[key] = true
			ev := &a2apkg.TaskStatusUpdateEvent{
				Metadata: meta,
				Status:   a2apkg.TaskStatus{State: a2apkg.TaskStateWorking},
			}
			require.Empty(t, mapA2AEvent(ev), "partial event must not emit a usage delta")
		})
	}
}

func TestMapA2AEvent_FailedTerminalCarriesUsage(t *testing.T) {
	for _, state := range []a2apkg.TaskState{a2apkg.TaskStateFailed, a2apkg.TaskStateRejected, a2apkg.TaskStateCanceled} {
		t.Run(string(state), func(t *testing.T) {
			ev := &a2apkg.TaskStatusUpdateEvent{
				Metadata: usageMeta(10, 5, 15),
				Status:   a2apkg.TaskStatus{State: state},
			}

			deltas := mapA2AEvent(ev)
			require.Len(t, deltas, 1)
			require.Error(t, deltas[0].Err)
			require.NotNil(t, deltas[0].Usage)
			require.Equal(t, TurnUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, *deltas[0].Usage)
		})
	}
}

func TestMapA2AEvent_NonPartialWorkingEventEmitsUsage(t *testing.T) {
	meta := usageMeta(3, 4, 7)
	meta[mdPartialADK] = false
	ev := &a2apkg.TaskStatusUpdateEvent{
		Metadata: meta,
		Status:   a2apkg.TaskStatus{State: a2apkg.TaskStateWorking},
	}
	deltas := mapA2AEvent(ev)
	require.Len(t, deltas, 1)
	require.NotNil(t, deltas[0].Usage)
	require.Equal(t, 7, deltas[0].Usage.TotalTokens)
}
