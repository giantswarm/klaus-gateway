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
	require.Equal(t, TurnUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, *deltas[0].Usage)
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

func TestOutboundDelta_IsZero(t *testing.T) {
	require.True(t, OutboundDelta{}.isZero())
	require.False(t, OutboundDelta{Usage: &TurnUsage{}}.isZero())
	require.False(t, OutboundDelta{Kind: DeltaToolActivity, Content: "x"}.isZero())
	require.False(t, OutboundDelta{Tool: &ToolActivity{Name: "x"}}.isZero())
}
