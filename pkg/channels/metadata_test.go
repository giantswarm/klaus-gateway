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

// The keys of an event's own metadata and of its status message's, sorted,
// and never a value.
func TestEventMetadataKeys(t *testing.T) {
	ev := &a2apkg.TaskStatusUpdateEvent{
		Metadata: map[string]any{mdUsageCanonical: map[string]any{"promptTokenCount": 3}, "kagent.dev/a2a/timeline-position": "x"},
		Status: a2apkg.TaskStatus{
			State:   a2apkg.TaskStateWorking,
			Message: &a2apkg.Message{Metadata: map[string]any{mdTypeKagent: mdTypeFunctionCall}},
		},
	}
	require.Equal(t, []string{"kagent.dev/a2a/timeline-position", mdUsageCanonical, "message." + mdTypeKagent}, eventMetadataKeys(ev))

	task := &a2apkg.Task{
		Metadata: map[string]any{"kagent.dev/a2a/task-created-at": "x"},
		Status: a2apkg.TaskStatus{
			State:   a2apkg.TaskStateCompleted,
			Message: &a2apkg.Message{Metadata: map[string]any{mdUsageCanonical: map[string]any{}}},
		},
	}
	require.Equal(t, []string{"kagent.dev/a2a/task-created-at", "message." + mdUsageCanonical}, eventMetadataKeys(task),
		"a whole task's status message is a carrier too")

	artifact := &a2apkg.TaskArtifactUpdateEvent{
		Metadata: map[string]any{"kagent.dev/a2a/timeline-position": "x"},
		Artifact: &a2apkg.Artifact{Metadata: map[string]any{mdUsageCanonical: map[string]any{}}},
	}
	require.Equal(t, []string{"artifact." + mdUsageCanonical, "kagent.dev/a2a/timeline-position"}, eventMetadataKeys(artifact),
		"an artifact update's artifact is a carrier too")

	require.Empty(t, eventMetadataKeys(&a2apkg.Task{}), "an event without metadata lists no key")
}

func usageMeta(prompt, completion, total float64) map[string]any {
	return map[string]any{
		mdUsageCanonical: map[string]any{
			usagePromptTokens:     prompt,
			usageCompletionTokens: completion,
			usageTotalTokens:      total,
		},
	}
}

// kagent puts each LLM call's usage on the artifact of an artifact update,
// beside the text that call produced; it maps to one usage-only delta.
func TestMapA2AEvent_ArtifactCarriesUsage(t *testing.T) {
	ev := &a2apkg.TaskArtifactUpdateEvent{
		Artifact: &a2apkg.Artifact{
			ID:       "a1",
			Metadata: usageMeta(10, 5, 15),
			Parts:    a2apkg.ContentParts{a2apkg.NewTextPart("16 nodes")},
		},
	}

	deltas := newEventMapper().deltas(ev)
	require.Len(t, deltas, 2)
	require.Equal(t, DeltaText, deltas[0].Kind)
	require.Equal(t, "16 nodes", deltas[0].Content)
	require.NotNil(t, deltas[1].Usage)
	require.Equal(t, TurnUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, *deltas[1].Usage)
}

// One usage delta per artifact that carries usage, so a turn of four LLM calls
// sums four calls, never one call twice.
func TestMapA2AEvent_OneUsageDeltaPerCall(t *testing.T) {
	m := newEventMapper()
	var usages []TurnUsage
	for i := range 4 {
		ev := &a2apkg.TaskArtifactUpdateEvent{Artifact: &a2apkg.Artifact{
			ID:       a2apkg.ArtifactID(string(rune('a' + i))),
			Metadata: usageMeta(float64(10*(i+1)), 1, float64(10*(i+1)+1)),
		}}
		for _, d := range m.deltas(ev) {
			if d.Usage != nil {
				usages = append(usages, *d.Usage)
			}
		}
		// An artifact update without usage adds none.
		for _, d := range m.deltas(&a2apkg.TaskArtifactUpdateEvent{Artifact: &a2apkg.Artifact{ID: "plain"}}) {
			require.Nil(t, d.Usage)
		}
	}
	require.Len(t, usages, 4)
	require.Equal(t, 100, usages[0].InputTokens+usages[1].InputTokens+usages[2].InputTokens+usages[3].InputTokens)
}

// Usage is read from the artifact only: a status update or a whole task that
// carried the key would be a second copy of a call already counted.
func TestMapA2AEvent_UsageOnlyFromTheArtifact(t *testing.T) {
	working := &a2apkg.TaskStatusUpdateEvent{
		Metadata: usageMeta(3, 4, 7),
		Status: a2apkg.TaskStatus{
			State:   a2apkg.TaskStateWorking,
			Message: &a2apkg.Message{Metadata: usageMeta(3, 4, 7)},
		},
	}
	require.Empty(t, newEventMapper().deltas(working))

	completed := &a2apkg.TaskStatusUpdateEvent{
		Metadata: usageMeta(10, 5, 15),
		Status:   a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
	}
	deltas := newEventMapper().deltas(completed)
	require.Len(t, deltas, 1)
	require.True(t, deltas[0].Done)
	require.Nil(t, deltas[0].Usage)

	failed := &a2apkg.TaskStatusUpdateEvent{
		Metadata: usageMeta(10, 5, 15),
		Status:   a2apkg.TaskStatus{State: a2apkg.TaskStateFailed},
	}
	deltas = newEventMapper().deltas(failed)
	require.Len(t, deltas, 1)
	require.Error(t, deltas[0].Err)
	require.Nil(t, deltas[0].Usage)
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

	deltas := newEventMapper().deltas(ev)
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

func TestMapA2AEvent_InterimToolActivity(t *testing.T) {
	resp := dataPart(t, mdTypeFunctionResponse, map[string]any{
		"name":     "kubectl_get",
		"response": map[string]any{"output": "pod/foo"},
		"id":       "call-1",
	})
	ev := &a2apkg.TaskStatusUpdateEvent{
		Status: a2apkg.TaskStatus{
			State:   a2apkg.TaskStateWorking,
			Message: &a2apkg.Message{Parts: a2apkg.ContentParts{resp}},
		},
	}

	deltas := newEventMapper().deltas(ev)
	require.Len(t, deltas, 1)
	require.Equal(t, DeltaToolActivity, deltas[0].Kind)
	require.Equal(t, ToolResult, deltas[0].Tool.Kind)
	require.Equal(t, "kubectl_get", deltas[0].Tool.Name)
}

func TestMapA2AEvent_ConfirmationPartIsNotToolActivity(t *testing.T) {
	// A long-running function_call is the runtime's confirmation bookkeeping
	// (HITL), routed via input-required, not surfaced as tool activity.
	confirm := a2apkg.NewDataPart(map[string]any{"name": "adk_request_confirmation"})
	confirm.Metadata = map[string]any{mdTypeKagent: mdTypeFunctionCall, mdLongRunningKagent: true}
	ev := &a2apkg.TaskArtifactUpdateEvent{
		Artifact: &a2apkg.Artifact{Parts: a2apkg.ContentParts{confirm}},
	}
	require.Empty(t, newEventMapper().deltas(ev))
}

// A call to a tool that needs approval is answered by the runtime with its
// confirmation request before the task pauses; that result is marked, any
// other result is not.
func TestMapA2AEvent_ConfirmationRequiredResultAwaitsApproval(t *testing.T) {
	result := func(resp map[string]any) *ToolActivity {
		t.Helper()
		part := dataPart(t, mdTypeFunctionResponse, map[string]any{"name": "call_tool", "id": "call-1", "response": resp})
		ev := &a2apkg.TaskArtifactUpdateEvent{Artifact: &a2apkg.Artifact{Parts: a2apkg.ContentParts{part}}}
		deltas := newEventMapper().deltas(ev)
		require.Len(t, deltas, 1)
		require.NotNil(t, deltas[0].Tool)
		return deltas[0].Tool
	}

	held := result(map[string]any{"error": `error tool "call_tool" requires confirmation, please approve or reject`})
	require.Equal(t, ToolResult, held.Kind)
	require.True(t, held.AwaitsApproval)

	require.False(t, result(map[string]any{"error": "connection refused"}).AwaitsApproval)
	require.False(t, result(map[string]any{"output": "3 pods"}).AwaitsApproval)
}

// The prose the agent writes before firing its tool calls rides on the same
// working event as the calls, and must reach the channel ahead of them
// (klaus-gateway#197).
func TestMapA2AEvent_NarrationPrecedesToolActivity(t *testing.T) {
	call := func(id string) *a2apkg.Part {
		return dataPart(t, mdTypeFunctionCall, map[string]any{"name": "kubectl_get", "id": id})
	}
	ev := &a2apkg.TaskStatusUpdateEvent{
		Status: a2apkg.TaskStatus{
			State: a2apkg.TaskStateWorking,
			Message: a2apkg.NewMessage(a2apkg.MessageRoleAgent,
				a2apkg.NewTextPart("Let me pull the HelmRelease from both clusters simultaneously."),
				call("call-1"), call("call-2")),
		},
	}

	deltas := newEventMapper().deltas(ev)
	require.Len(t, deltas, 3)
	require.Equal(t, DeltaNarration, deltas[0].Kind)
	require.Equal(t, "Let me pull the HelmRelease from both clusters simultaneously.", deltas[0].Content)
	require.Equal(t, DeltaToolActivity, deltas[1].Kind)
	require.Equal(t, "call-1", deltas[1].Tool.CallID)
	require.Equal(t, DeltaToolActivity, deltas[2].Kind)
	require.Equal(t, "call-2", deltas[2].Tool.CallID)
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
	require.Empty(t, newEventMapper().deltas(ev))
}

// kagent echoes the inbound user message as the submitted event of a new task.
func TestMapA2AEvent_UserEchoEmitsNothing(t *testing.T) {
	ev := &a2apkg.TaskStatusUpdateEvent{
		Status: a2apkg.TaskStatus{
			State:   a2apkg.TaskStateSubmitted,
			Message: a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("compare both clusters")),
		},
	}
	require.Empty(t, newEventMapper().deltas(ev))
}

// Tool results arrive as their own data-only events, so text beside a
// function_response is not narration. Keeping it out also keeps narration from
// being rendered before the same message's tool payload has taught the writer
// which login URLs to scrub.
func TestMapA2AEvent_TextBesideToolResultEmitsNoNarration(t *testing.T) {
	resp := dataPart(t, mdTypeFunctionResponse, map[string]any{
		"name":     "core_auth_login",
		"id":       "call-1",
		"response": map[string]any{"output": "Server: pro\nhttps://auth.example/authorize?x=1"},
	})
	ev := &a2apkg.TaskStatusUpdateEvent{
		Status: a2apkg.TaskStatus{
			State: a2apkg.TaskStateWorking,
			Message: a2apkg.NewMessage(a2apkg.MessageRoleAgent,
				a2apkg.NewTextPart("sign in at https://auth.example/authorize?x=1"), resp),
		},
	}

	deltas := newEventMapper().deltas(ev)
	require.Len(t, deltas, 1)
	require.Equal(t, DeltaToolActivity, deltas[0].Kind)
}

// A whole task arrives as the first event of a stream (the submitted snapshot)
// and as the controller's answer from its store; only a quiescent state renders.
func TestMapA2AEvent_TaskSnapshots(t *testing.T) {
	submitted := &a2apkg.Task{ID: "t1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}}
	require.Empty(t, newEventMapper().deltas(submitted), "the submitted snapshot is the turn starting, nothing to render")

	paused := &a2apkg.Task{ID: "t1", Status: a2apkg.TaskStatus{
		State:   a2apkg.TaskStateInputRequired,
		Message: a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("approve?")),
	}}
	deltas := newEventMapper().deltas(paused)
	require.Len(t, deltas, 1)
	require.Equal(t, DeltaPrompt, deltas[0].Kind)
	require.Equal(t, "t1", deltas[0].TaskID)

	canceled := &a2apkg.Task{ID: "t1", Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCanceled}}
	deltas = newEventMapper().deltas(canceled)
	require.Len(t, deltas, 1)
	require.ErrorContains(t, deltas[0].Err, "TASK_STATE_CANCELED")
}

// A bare agent message is a complete reply without a task wrapper.
func TestMapA2AEvent_MessageIsACompleteReply(t *testing.T) {
	deltas := newEventMapper().deltas(a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("done")))
	require.Len(t, deltas, 2)
	require.Equal(t, "done", deltas[0].Content)
	require.True(t, deltas[1].Done)
}

func TestOutboundDelta_IsZero(t *testing.T) {
	require.True(t, OutboundDelta{}.isZero())
	require.False(t, OutboundDelta{Usage: &TurnUsage{}}.isZero())
	require.False(t, OutboundDelta{Kind: DeltaToolActivity, Content: "x"}.isZero())
	require.False(t, OutboundDelta{Tool: &ToolActivity{Name: "x"}}.isZero())
}

func artifactUpdate(id a2apkg.ArtifactID, appendTo, last bool, parts ...*a2apkg.Part) *a2apkg.TaskArtifactUpdateEvent {
	return &a2apkg.TaskArtifactUpdateEvent{
		TaskID:    "task-1",
		Append:    appendTo,
		LastChunk: last,
		Artifact:  &a2apkg.Artifact{ID: id, Parts: parts},
	}
}

// texts renders the events through one mapper and returns the text deltas.
func texts(t *testing.T, m *eventMapper, events ...a2apkg.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		for _, d := range m.deltas(ev) {
			if d.Kind == DeltaText && d.Content != "" {
				out = append(out, d.Content)
			}
		}
	}
	return out
}

// The Go ADK streams a text run as appended chunks and then re-sends the run
// whole on the same artifact (append false, lastChunk true). Rendered as
// appends, every run appeared twice (klaus-gateway#242); the replace renders
// only what the artifact has not delivered yet — nothing, when the run was
// streamed in full.
func TestEventMapper_ArtifactReplaceRendersTheRunOnce(t *testing.T) {
	got := texts(t, newEventMapper(),
		artifactUpdate("a", false, false, a2apkg.NewTextPart("I'll look for ")),
		artifactUpdate("a", true, false, a2apkg.NewTextPart("the right tools.")),
		artifactUpdate("a", false, true, a2apkg.NewTextPart("I'll look for the right tools.")),
	)
	require.Equal(t, []string{"I'll look for ", "the right tools."}, got)
}

// The Go ADK streams a run as append chunks and re-sends it whole on the same
// artifact. With the call's usage on that final replace, the run counts its
// call once: one usage delta, its text not repeated.
func TestEventMapper_ArtifactReplaceCountsTheCallOnce(t *testing.T) {
	m := newEventMapper()
	final := artifactUpdate("a", false, true, a2apkg.NewTextPart("I'll look for the right tools."))
	final.Artifact.Metadata = usageMeta(120, 30, 150)
	var usages []TurnUsage
	var text []string
	for _, ev := range []a2apkg.Event{
		artifactUpdate("a", false, false, a2apkg.NewTextPart("I'll look for ")),
		artifactUpdate("a", true, false, a2apkg.NewTextPart("the right tools.")),
		final,
	} {
		for _, d := range m.deltas(ev) {
			if d.Usage != nil {
				usages = append(usages, *d.Usage)
			}
			if d.Kind == DeltaText && d.Content != "" {
				text = append(text, d.Content)
			}
		}
	}
	require.Equal(t, []TurnUsage{{InputTokens: 120, OutputTokens: 30, TotalTokens: 150}}, usages)
	require.Equal(t, []string{"I'll look for ", "the right tools."}, text)
}

func usagesOf(deltas []OutboundDelta) []TurnUsage {
	var usages []TurnUsage
	for _, d := range deltas {
		if d.Usage != nil {
			usages = append(usages, *d.Usage)
		}
	}
	return usages
}

func completedTaskWithUsage() *a2apkg.Task {
	return &a2apkg.Task{
		ID:     "t1",
		Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted},
		Artifacts: []*a2apkg.Artifact{
			{ID: "a", Metadata: usageMeta(100, 20, 120), Parts: a2apkg.ContentParts{a2apkg.NewTextPart("Let me check.")}},
			{ID: "b", Metadata: usageMeta(200, 40, 240), Parts: a2apkg.ContentParts{a2apkg.NewTextPart("7 nodes.")}},
		},
	}
}

// A turn recovered as a whole completed task (start-up recovery, a
// resubscription) counts the usage each of its artifacts carries, ahead of
// the terminal delta.
func TestEventMapper_CompletedTaskCountsArtifactUsage(t *testing.T) {
	deltas := newEventMapper().deltas(completedTaskWithUsage())
	require.Equal(t, []TurnUsage{
		{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
		{InputTokens: 200, OutputTokens: 40, TotalTokens: 240},
	}, usagesOf(deltas))
	require.True(t, deltas[len(deltas)-1].Done)
}

// A completed task after a stream that delivered its artifacts' usage counts
// none of it again.
func TestEventMapper_CompletedTaskAfterStreamCountsNoUsage(t *testing.T) {
	m := newEventMapper()
	task := completedTaskWithUsage()
	for _, artifact := range task.Artifacts {
		require.Len(t, usagesOf(m.deltas(&a2apkg.TaskArtifactUpdateEvent{Artifact: artifact})), 1)
	}
	deltas := m.deltas(task)
	require.Empty(t, usagesOf(deltas))
	require.True(t, deltas[len(deltas)-1].Done)
}

// The finished run's replace carries the tool calls the run ended in; those
// still render as tool activity even though the text adds nothing.
func TestEventMapper_ArtifactReplaceKeepsToolActivity(t *testing.T) {
	m := newEventMapper()
	require.Len(t, texts(t, m, artifactUpdate("a", false, false, a2apkg.NewTextPart("Let me check."))), 1)
	call := dataPart(t, mdTypeFunctionCall, map[string]any{"name": "kubectl_get", "id": "call-1"})
	deltas := m.deltas(artifactUpdate("a", false, true, a2apkg.NewTextPart("Let me check."), call))
	require.Len(t, deltas, 1)
	require.Equal(t, DeltaToolActivity, deltas[0].Kind)
	require.Equal(t, "call-1", deltas[0].Tool.CallID)
}

// A run that was streamed short of its final text renders the remainder, and
// a replacement diverging from what was streamed renders what lies past the
// common prefix, so no text is lost either way.
func TestEventMapper_ArtifactReplaceRendersTheRemainder(t *testing.T) {
	got := texts(t, newEventMapper(),
		artifactUpdate("a", false, false, a2apkg.NewTextPart("7 nodes, all Re")),
		artifactUpdate("a", false, true, a2apkg.NewTextPart("7 nodes, all Ready.")),
	)
	require.Equal(t, []string{"7 nodes, all Re", "ady."}, got)

	got = texts(t, newEventMapper(),
		artifactUpdate("a", false, false, a2apkg.NewTextPart("Hello wörld")),
		artifactUpdate("a", false, true, a2apkg.NewTextPart("Hello wörd!")),
	)
	require.Equal(t, []string{"Hello wörld", "d!"}, got)
}

// Text runs come on separate artifacts (a tool call closes a run); they are
// separated by a paragraph so the runs do not run into each other. A run that
// already ends its paragraph gets no extra newline.
func TestEventMapper_NewArtifactOpensAParagraph(t *testing.T) {
	got := texts(t, newEventMapper(),
		artifactUpdate("a", false, false, a2apkg.NewTextPart("Let me first discover what's available.")),
		artifactUpdate("a", false, true, a2apkg.NewTextPart("Let me first discover what's available.")),
		artifactUpdate("b", false, true, a2apkg.NewTextPart("`x_kubernetes_cluster_health` looks ideal.\n")),
		artifactUpdate("c", false, true, a2apkg.NewTextPart("7 nodes, all Ready.")),
	)
	require.Equal(t, []string{
		"Let me first discover what's available.",
		"\n\n`x_kubernetes_cluster_health` looks ideal.\n",
		"\n7 nodes, all Ready.",
	}, got)
}

// An artifact without an ID cannot be reconciled and renders as sent; an
// append to an artifact never seen renders as sent too.
func TestEventMapper_UnaddressableArtifactsRenderAsSent(t *testing.T) {
	got := texts(t, newEventMapper(),
		artifactUpdate("", false, false, a2apkg.NewTextPart("one")),
		artifactUpdate("", false, true, a2apkg.NewTextPart("one")),
		artifactUpdate("z", true, false, a2apkg.NewTextPart(" two")),
	)
	require.Equal(t, []string{"one", "one", "\n\n two"}, got)
}

func TestCommonPrefixLen(t *testing.T) {
	require.Equal(t, 0, commonPrefixLen("", "abc"))
	require.Equal(t, 3, commonPrefixLen("abc", "abcdef"))
	require.Equal(t, 3, commonPrefixLen("abcdef", "abc"))
	require.Equal(t, 2, commonPrefixLen("abX", "abY"))
	// ö is 2 bytes; a divergence inside it backs off to the rune start.
	require.Equal(t, 1, commonPrefixLen("aö", "aü"))
}

// kagent 1.1 and later mark tool activity with the canonical kagent.dev/a2a/
// key only; older releases use the kagent_ or adk_ prefix. Every spelling maps
// to the same delta.
func TestMapA2AEvent_ReadsEveryPartTypeKeySpelling(t *testing.T) {
	for _, typeKey := range []string{mdTypeCanonical, mdTypeKagent, mdTypeADK} {
		t.Run(typeKey, func(t *testing.T) {
			call := a2apkg.NewDataPart(map[string]any{"name": "kubectl_get", "id": "call-1"})
			call.Metadata = map[string]any{typeKey: mdTypeFunctionCall}
			ev := &a2apkg.TaskStatusUpdateEvent{
				Status: a2apkg.TaskStatus{
					State:   a2apkg.TaskStateWorking,
					Message: &a2apkg.Message{Parts: a2apkg.ContentParts{call}},
				},
			}

			deltas := newEventMapper().deltas(ev)
			require.Len(t, deltas, 1)
			require.Equal(t, DeltaToolActivity, deltas[0].Kind)
			require.Equal(t, ToolCall, deltas[0].Tool.Kind)
			require.Equal(t, "kubectl_get", deltas[0].Tool.Name)
		})
	}
}

// The canonical metadata carries no long-running marker, so the runtime's
// confirmation call is recognised by its name.
func TestMapA2AEvent_CanonicalConfirmationPartIsNotToolActivity(t *testing.T) {
	confirm := a2apkg.NewDataPart(map[string]any{"name": confirmationCallName, "id": "confirm-1"})
	confirm.Metadata = map[string]any{mdTypeCanonical: mdTypeFunctionCall}
	ev := &a2apkg.TaskArtifactUpdateEvent{
		Artifact: &a2apkg.Artifact{Parts: a2apkg.ContentParts{confirm}},
	}
	require.Empty(t, newEventMapper().deltas(ev))
}
