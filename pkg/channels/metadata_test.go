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

	deltas := newEventMapper().deltas(ev)
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

	deltas := newEventMapper().deltas(ev)
	require.Len(t, deltas, 2)
	require.Equal(t, DeltaToolActivity, deltas[0].Kind)
	require.Equal(t, ToolResult, deltas[0].Tool.Kind)
	require.Equal(t, "kubectl_get", deltas[0].Tool.Name)
	require.NotNil(t, deltas[1].Usage)
	require.Equal(t, 7, deltas[1].Usage.TotalTokens)
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

	deltas := newEventMapper().deltas(ev)
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

				deltas := newEventMapper().deltas(ev)
				require.Len(t, deltas, 1)
				require.Equal(t, DeltaToolActivity, deltas[0].Kind)
			})
		}
	}
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

// A partial chunk mirrors the usage of the LLM call it belongs to, wherever
// kagent stamped the flag; counting it would tally one call several times.
func TestMapA2AEvent_MessagePartialUsageSkipped(t *testing.T) {
	for _, key := range []string{mdPartialKagent, mdPartialADK} {
		t.Run(key, func(t *testing.T) {
			msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("thinking"))
			msg.Metadata = usageMeta(3, 4, 7)
			msg.Metadata[key] = true
			ev := &a2apkg.TaskStatusUpdateEvent{
				Status: a2apkg.TaskStatus{State: a2apkg.TaskStateWorking, Message: msg},
			}
			require.Empty(t, newEventMapper().deltas(ev), "usage on a partial message must not be counted")
		})
	}
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

// An adapter that concatenates chunks into one reply must not glue narration to
// the answer that follows it.
func TestOutboundDelta_StreamText(t *testing.T) {
	require.Equal(t, "let me look\n\n", OutboundDelta{Kind: DeltaNarration, Content: "let me look"}.StreamText())
	require.Equal(t, "the answer", OutboundDelta{Content: "the answer"}.StreamText())
	require.Empty(t, OutboundDelta{Kind: DeltaNarration}.StreamText())
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
			require.Empty(t, newEventMapper().deltas(ev), "partial event must not emit a usage delta")
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

			deltas := newEventMapper().deltas(ev)
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
	deltas := newEventMapper().deltas(ev)
	require.Len(t, deltas, 1)
	require.NotNil(t, deltas[0].Usage)
	require.Equal(t, 7, deltas[0].Usage.TotalTokens)
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
