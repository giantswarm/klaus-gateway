package channels_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/instance"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

type fakeLifecycle struct {
	instances map[string]lifecycle.InstanceRef
}

func (f *fakeLifecycle) Get(_ context.Context, name string) (lifecycle.InstanceRef, error) {
	if ref, ok := f.instances[name]; ok {
		return ref, nil
	}
	return lifecycle.InstanceRef{}, lifecycle.ErrNotFound
}
func (f *fakeLifecycle) Create(_ context.Context, s lifecycle.CreateSpec) (lifecycle.InstanceRef, error) {
	ref := lifecycle.InstanceRef{Name: s.Name, BaseURL: "http://" + s.Name, Status: "ready"}
	if f.instances == nil {
		f.instances = map[string]lifecycle.InstanceRef{}
	}
	f.instances[s.Name] = ref
	return ref, nil
}
func (f *fakeLifecycle) List(context.Context) ([]lifecycle.InstanceRef, error) { return nil, nil }
func (f *fakeLifecycle) Stop(context.Context, string) error                    { return nil }

type fakeClient struct {
	sseBody  string
	messages []instance.Message
	err      error
}

func (f *fakeClient) StreamCompletion(context.Context, channels.InstanceRef, []byte) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.sseBody)), nil
}
func (f *fakeClient) Messages(context.Context, channels.InstanceRef, string) (instance.MessagesResponse, error) {
	return instance.MessagesResponse{Messages: f.messages}, nil
}

func TestFacade_ResolveCreatesInstance(t *testing.T) {
	s := memory.New()
	lm := &fakeLifecycle{}
	router := routing.New(s, lm, true, time.Hour)
	f := &channels.Facade{Router: router}

	ref, err := f.Resolve(context.Background(), channels.InboundMessage{
		Channel:   "web",
		ChannelID: "c1",
		UserID:    "u1",
		ThreadID:  "t1",
	})
	require.NoError(t, err)
	require.NotEmpty(t, ref.Name)

	// And the store now has the mapping.
	entries, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, ref.Name, entries[0].Entry.Instance)
}

func TestFacade_ResolveAutoCreateOffReturnsRouteNotFound(t *testing.T) {
	s := memory.New()
	lm := &fakeLifecycle{}
	router := routing.New(s, lm, false, time.Hour)
	f := &channels.Facade{Router: router}

	_, err := f.Resolve(context.Background(), channels.InboundMessage{
		Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1",
	})
	require.ErrorIs(t, err, routing.ErrRouteNotFound)
}

func TestFacade_SendCompletionEmitsDeltas(t *testing.T) {
	client := &fakeClient{sseBody: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
		"data: [DONE]\n\n"}
	f := &channels.Facade{Client: client}

	ch, err := f.SendCompletion(context.Background(), channels.InstanceRef{Name: "i1"}, channels.InboundMessage{Text: "hi"})
	require.NoError(t, err)

	var parts []string
	var done bool
	for d := range ch {
		if d.Err != nil {
			t.Fatalf("unexpected error: %v", d.Err)
		}
		if d.Done {
			done = true
			continue
		}
		if d.Content != "" {
			parts = append(parts, d.Content)
		}
	}
	require.True(t, done, "expected terminal Done delta")
	require.Equal(t, "hello world", strings.Join(parts, ""))
}

func TestFacade_SendCompletionSurfacesUpstreamError(t *testing.T) {
	client := &fakeClient{err: errors.New("upstream boom")}
	f := &channels.Facade{Client: client}

	_, err := f.SendCompletion(context.Background(), channels.InstanceRef{Name: "i1"}, channels.InboundMessage{Text: "hi"})
	require.Error(t, err)
}

func TestFacade_FetchHistory(t *testing.T) {
	client := &fakeClient{messages: []instance.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}}
	f := &channels.Facade{Client: client}

	msgs, err := f.FetchHistory(context.Background(), channels.InstanceRef{Name: "i1"})
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "user", msgs[0].Role)
	require.Equal(t, "hello", msgs[1].Content)
}

// fakeAgent is the kagent client at the facade's seam: it records the message
// and instance every turn is sent with, hands out instances keyed by request
// id like the controller's idempotent create, and plays back a fixed event
// sequence.
type fakeAgent struct {
	mu sync.Mutex

	events    []a2apkg.Event
	streamErr error // yielded as the stream's first item
	tailErr   error // yielded after events
	hold      chan struct{}

	instances map[string]pkga2a.Instance
	byRequest map[string]string
	createErr error
	getErr    error
	deleteErr error
	agents    []pkga2a.AgentInfo
	tasks     map[a2apkg.TaskID]*a2apkg.Task

	streamCtx       context.Context
	streamed        []*a2apkg.Message
	streamedOn      []string
	createRequests  []string
	canceled        []a2apkg.TaskID
	canceledOn      []string
	deleted         []string
	gotTasks        []a2apkg.TaskID
	created, gotIns int
}

func newFakeAgent(events ...a2apkg.Event) *fakeAgent {
	return &fakeAgent{
		events:    events,
		instances: map[string]pkga2a.Instance{},
		byRequest: map[string]string{},
		tasks:     map[a2apkg.TaskID]*a2apkg.Task{},
	}
}

func (a *fakeAgent) Stream(ctx context.Context, instanceID string, msg *a2apkg.Message) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		a.mu.Lock()
		a.streamCtx = ctx
		a.streamed = append(a.streamed, msg)
		a.streamedOn = append(a.streamedOn, instanceID)
		events, streamErr, tailErr, hold := a.events, a.streamErr, a.tailErr, a.hold
		a.mu.Unlock()
		if streamErr != nil {
			yield(nil, streamErr)
			return
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
		if tailErr != nil {
			yield(nil, tailErr)
		}
	}
}

func (a *fakeAgent) GetTask(_ context.Context, _ string, taskID a2apkg.TaskID) (*a2apkg.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gotTasks = append(a.gotTasks, taskID)
	task, ok := a.tasks[taskID]
	if !ok {
		return nil, a2apkg.ErrTaskNotFound
	}
	return task, nil
}

func (a *fakeAgent) CancelTask(_ context.Context, instanceID string, taskID a2apkg.TaskID) (*a2apkg.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.canceled = append(a.canceled, taskID)
	a.canceledOn = append(a.canceledOn, instanceID)
	return &a2apkg.Task{ID: taskID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCanceled}}, nil
}

func (a *fakeAgent) CreateInstance(_ context.Context, agentRef, requestID string) (pkga2a.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.createRequests = append(a.createRequests, requestID)
	if a.createErr != nil {
		return pkga2a.Instance{}, a.createErr
	}
	if id, ok := a.byRequest[requestID]; ok {
		return a.instances[id], nil
	}
	a.created++
	inst := pkga2a.Instance{ID: fmt.Sprintf("inst-%s-%d", agentRef, a.created), State: "AGENT_INSTANCE_STATE_READY"}
	a.instances[inst.ID] = inst
	a.byRequest[requestID] = inst.ID
	return inst, nil
}

func (a *fakeAgent) GetInstance(_ context.Context, id string) (pkga2a.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gotIns++
	if a.getErr != nil {
		return pkga2a.Instance{}, a.getErr
	}
	inst, ok := a.instances[id]
	if !ok {
		return pkga2a.Instance{}, fmt.Errorf("%w: %s", pkga2a.ErrInstanceNotFound, id)
	}
	return inst, nil
}

func (a *fakeAgent) DeleteInstance(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deleted = append(a.deleted, id)
	if a.deleteErr != nil {
		return a.deleteErr
	}
	delete(a.instances, id)
	return nil
}

func (a *fakeAgent) ListAgents(context.Context) ([]pkga2a.AgentInfo, error) {
	return a.agents, nil
}

func (a *fakeAgent) lastStreamed() *a2apkg.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streamed[len(a.streamed)-1]
}

// newA2AFacade wires a facade on the fake with a memory store.
func newA2AFacade(agent *fakeAgent) (*channels.Facade, store.Store) {
	s := memory.New()
	return &channels.Facade{Agent: agent, Routes: s}, s
}

// taskInfo is the identity of the fake turn's task, as the controller's events
// would carry it.
var taskInfo = a2apkg.TaskInfo{TaskID: "task-1", ContextID: "ctx-1"}

func slackMsg(text string) channels.InboundMessage {
	return channels.InboundMessage{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", UserID: "U1", AgentRef: "kagent/worker", Text: text, BearerToken: "user-jwt"}
}

// drain collects every delta of a turn.
func drain(t *testing.T, ch <-chan channels.OutboundDelta) []channels.OutboundDelta {
	t.Helper()
	var deltas []channels.OutboundDelta
	for d := range ch {
		deltas = append(deltas, d)
	}
	return deltas
}

func TestFacade_SendCompletionViaA2A_FirstTurnCreatesTheInstance(t *testing.T) {
	agent := newFakeAgent(
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart("hello world")),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	)
	f, routes := newA2AFacade(agent)
	msg := slackMsg("hi")

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, msg)
	require.NoError(t, err)
	var content strings.Builder
	var done bool
	for _, d := range drain(t, ch) {
		require.NoError(t, d.Err)
		if d.Done {
			done = true
		}
		content.WriteString(d.Content)
	}
	require.Equal(t, "hello world", content.String())
	require.True(t, done)

	// The instance was created with the synthesized context id as the
	// idempotency key (thread-scoped: empty user slot), the turn ran on it,
	// and the binding is persisted for the next turn and the next process.
	wantRequest := channels.SynthesizeContextID("slack", "C1", "", "1700.0001", "kagent/worker")
	require.Equal(t, []string{wantRequest}, agent.createRequests)
	require.Equal(t, []string{"inst-kagent/worker-1"}, agent.streamedOn)
	entry, ok, err := routes.Get(t.Context(), store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", Agent: "kagent/worker"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "inst-kagent/worker-1", entry.AgentInstanceID)
	require.Empty(t, entry.Instance)

	sent := agent.lastStreamed()
	require.Empty(t, sent.TaskID, "a fresh turn lets the controller assign the task id")
	require.Empty(t, sent.ContextID, "the controller owns the conversation's context id")
	require.Equal(t, a2apkg.MessageRoleUser, sent.Role)
	require.Equal(t, "hi", sent.Parts[0].Text())
}

func TestFacade_SendCompletionViaA2A_LaterTurnsReuseTheBinding(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, routes := newA2AFacade(agent)
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", Agent: "kagent/worker"}
	require.NoError(t, routes.Put(t.Context(), key, store.Entry{AgentInstanceID: "inst-from-before-the-restart"}))

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("and the nodes?"))
	require.NoError(t, err)
	drain(t, ch)

	require.Empty(t, agent.createRequests, "a bound thread creates no instance")
	require.Equal(t, []string{"inst-from-before-the-restart"}, agent.streamedOn)
}

// A retried first turn (the binding was not written, or the process died in
// between) reaches the controller with the same request id and gets the same
// instance back instead of a second one.
func TestFacade_SendCompletionViaA2A_RetriedFirstTurnIsIdempotent(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, routes := newA2AFacade(agent)
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", Agent: "kagent/worker"}

	for range 2 {
		ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("hi"))
		require.NoError(t, err)
		drain(t, ch)
		require.NoError(t, routes.Delete(t.Context(), key))
	}
	require.Len(t, agent.createRequests, 2)
	require.Equal(t, agent.createRequests[0], agent.createRequests[1])
	require.Equal(t, 1, agent.created, "the same request id yields the same instance")
	require.Equal(t, []string{"inst-kagent/worker-1", "inst-kagent/worker-1"}, agent.streamedOn)
}

// The issue's turn: three rounds of "narrate, then call tools", then the final
// answer, which kagent sends twice — as a text-only working event and as the
// artifact. Every narration must arrive exactly once, and the answer must not be
// duplicated (klaus-gateway#197).
func TestFacade_SendCompletionViaA2A_NarrationDeliveredOnceWithFinalAnswer(t *testing.T) {
	const (
		narration1 = "Let me pull the HelmRelease from both clusters simultaneously."
		narration2 = "Both HelmReleases share the same chart version — the differences will be in the ConfigMaps."
		answer     = "Here is the focused diff on the klausGateway section:"
	)
	working := func(parts ...*a2apkg.Part) a2apkg.Event {
		msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, parts...)
		return a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, msg)
	}
	call := func(id string) *a2apkg.Part { return kagentPart("function_call", "kubectl_get", id) }
	resp := func(id string) *a2apkg.Part { return kagentPart("function_response", "kubectl_get", id) }
	user := a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("compare both clusters"))

	agent := newFakeAgent(
		// The controller yields the stored submitted task first, then echoes the
		// user's own message as the submitted event.
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateSubmitted, user),
		working(a2apkg.NewTextPart(narration1), call("c1"), call("c2")),
		working(resp("c1"), resp("c2")),
		working(a2apkg.NewTextPart(narration2), call("c3")),
		working(resp("c3")),
		working(a2apkg.NewTextPart(answer)), // the mirror of the final answer
		a2apkg.NewArtifactEvent(taskInfo, a2apkg.NewTextPart(answer)),
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil),
	)
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("compare both clusters"))
	require.NoError(t, err)

	var narrations, texts []string
	var tools int
	var done bool
	for _, d := range drain(t, ch) {
		require.NoError(t, d.Err)
		switch {
		case d.Done:
			done = true
		case d.Kind == channels.DeltaNarration:
			narrations = append(narrations, d.Content)
		case d.Kind == channels.DeltaToolActivity:
			tools++
		case d.Content != "":
			texts = append(texts, d.Content)
		}
	}

	require.True(t, done)
	require.Equal(t, []string{narration1, narration2}, narrations)
	require.Equal(t, []string{answer}, texts, "the answer arrives once, from the artifact")
	require.Equal(t, 6, tools)
}

// kagentPart builds a kagent function_call/function_response DataPart the way
// the wire format spells it.
func kagentPart(kagentType, name, id string) *a2apkg.Part {
	p := a2apkg.NewDataPart(map[string]any{"name": name, "id": id})
	p.Metadata = map[string]any{"kagent_type": kagentType}
	return p
}

func TestFacade_SendCompletionViaA2A_ForwardsIdentity(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	f, _ := newA2AFacade(agent)

	msg := slackMsg("hi")
	msg.Channel = "web"
	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, msg)
	require.NoError(t, err)
	drain(t, ch)

	require.Equal(t, "kagent/worker", pkga2a.AgentRefFromContext(agent.streamCtx))
	require.Equal(t, "user-jwt", pkga2a.ForwardedTokenFromContext(agent.streamCtx))
	require.Equal(t, "web", pkga2a.ChannelFromContext(agent.streamCtx))
}

// A refusal before the first event — the controller rejecting the turn — is
// returned synchronously so channels render it as the turn's outcome.
func TestFacade_SendCompletionViaA2A_RefusalIsSynchronous(t *testing.T) {
	agent := newFakeAgent()
	agent.streamErr = fmt.Errorf("%w: AgentInstance x already has an active task", pkga2a.ErrInstanceBusy)
	f, _ := newA2AFacade(agent)

	_, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("hi"))
	require.ErrorIs(t, err, pkga2a.ErrInstanceBusy)

	agent = newFakeAgent()
	agent.createErr = fmt.Errorf("%w: kagent/worker: no Harness admits this AgentTemplate", pkga2a.ErrAgentUnavailable)
	f, _ = newA2AFacade(agent)
	_, err = f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("hi"))
	require.ErrorIs(t, err, pkga2a.ErrAgentUnavailable)
	require.Empty(t, agent.streamed, "a refused instance never gets a turn")
}

func TestFacade_SendCompletionViaA2A_MidStreamErrorPropagated(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, nil))
	agent.tailErr = errors.New("executor boom")
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("hi"))
	require.NoError(t, err)
	var gotErr error
	for _, d := range drain(t, ch) {
		if d.Err != nil {
			gotErr = d.Err
		}
	}
	require.ErrorContains(t, gotErr, "executor boom")
}

func TestFacade_SendCompletionFallsBackToOpenAI_WhenNoAgentRef(t *testing.T) {
	client := &fakeClient{sseBody: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"}
	f := &channels.Facade{Agent: newFakeAgent(), Client: client}

	// AgentRef is empty, so the OpenAI path must be used even though Agent is set.
	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{Name: "i1"}, channels.InboundMessage{Text: "hi"})
	require.NoError(t, err)

	var content strings.Builder
	for d := range ch {
		content.WriteString(d.Content)
	}
	require.Equal(t, "ok", content.String())
}

func TestFacade_SendCompletionViaA2A_InputRequired_EmitsPromptDelta(t *testing.T) {
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("approve the tool call?"))
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateInputRequired, msg))
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("hi"))
	require.NoError(t, err)
	deltas := drain(t, ch)
	require.Len(t, deltas, 1)
	require.Equal(t, channels.DeltaPrompt, deltas[0].Kind)
	require.Equal(t, "approve the tool call?", deltas[0].Content)
	require.Equal(t, string(taskInfo.TaskID), deltas[0].TaskID)
	require.Empty(t, agent.canceled, "a paused task is left paused, not canceled")
}

func TestFacade_SendCompletionViaA2A_AuthRequired_EmitsPromptDelta(t *testing.T) {
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateAuthRequired, nil))
	f, _ := newA2AFacade(agent)

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, slackMsg("hi"))
	require.NoError(t, err)
	deltas := drain(t, ch)
	require.Len(t, deltas, 1)
	require.Equal(t, channels.DeltaPrompt, deltas[0].Kind)
}

// A HITL decision resumes the paused task in place: the message carries the
// paused task's id, and the typed response is built against the request that
// task is paused on (the defect of #215 cannot recur on this transport).
func TestFacade_HitlResumeCarriesThePausedTaskAndTypedResponse(t *testing.T) {
	prompt := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("Delete the pod?"))
	require.NoError(t, pkga2a.AttachHITL(prompt, pkga2a.ToolApprovalRequest{
		Type:  pkga2a.HITLTypeToolApprovalRequest,
		Hint:  "Delete the pod?",
		Tools: []pkga2a.HITLTool{{ID: "approval-1", CallID: "toolu_1", Name: "kubectl_delete"}},
	}))
	agent := newFakeAgent(a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateCompleted, nil))
	agent.tasks[taskInfo.TaskID] = &a2apkg.Task{
		ID: taskInfo.TaskID, ContextID: taskInfo.ContextID,
		Status: a2apkg.TaskStatus{State: a2apkg.TaskStateInputRequired, Message: prompt},
	}
	f, routes := newA2AFacade(agent)
	require.NoError(t, routes.Put(t.Context(), store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", Agent: "kagent/worker"},
		store.Entry{AgentInstanceID: "inst-1"}))

	msg := slackMsg("approve")
	msg.TaskID = string(taskInfo.TaskID)
	msg.Decision = &channels.HitlDecision{Type: channels.DecisionApprove}
	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, msg)
	require.NoError(t, err)
	drain(t, ch)

	require.Equal(t, []a2apkg.TaskID{taskInfo.TaskID}, agent.gotTasks, "the paused task is read for its request")
	sent := agent.lastStreamed()
	require.Equal(t, taskInfo.TaskID, sent.TaskID, "the decision message carries the paused task id")
	require.Contains(t, sent.Extensions, pkga2a.HITLExtensionURI)
	payload, ok := sent.Metadata[pkga2a.HITLExtensionURI].(map[string]any)
	require.True(t, ok)
	require.Equal(t, pkga2a.HITLTypeToolApprovalResponse, payload["type"])
	approvals, ok := payload["approvals"].([]any)
	require.True(t, ok)
	require.Len(t, approvals, 1)
	require.Equal(t, map[string]any{"id": "approval-1", "approved": true}, approvals[0])
	require.Equal(t, "approve", sent.Parts[0].Text(), "the decision keeps a readable label in the history")
}

func TestFacade_HitlResumeRefusedWhenTheTaskIsNotPaused(t *testing.T) {
	agent := newFakeAgent()
	agent.tasks[taskInfo.TaskID] = &a2apkg.Task{ID: taskInfo.TaskID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateCompleted}}
	f, routes := newA2AFacade(agent)
	require.NoError(t, routes.Put(t.Context(), store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", Agent: "kagent/worker"},
		store.Entry{AgentInstanceID: "inst-1"}))

	msg := slackMsg("approve")
	msg.TaskID = string(taskInfo.TaskID)
	msg.Decision = &channels.HitlDecision{Type: channels.DecisionApprove}
	_, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, msg)
	require.ErrorIs(t, err, channels.ErrNoPendingPrompt)
	require.Empty(t, agent.streamed)
}

// Stopping a turn cancels the task at the controller, not only the client
// stream, so the agent stops working.
func TestFacade_StoppedTurnCancelsTheTaskServerSide(t *testing.T) {
	agent := newFakeAgent(
		&a2apkg.Task{ID: taskInfo.TaskID, ContextID: taskInfo.ContextID, Status: a2apkg.TaskStatus{State: a2apkg.TaskStateSubmitted}},
		a2apkg.NewStatusUpdateEvent(taskInfo, a2apkg.TaskStateWorking, nil),
	)
	agent.hold = make(chan struct{})
	f, _ := newA2AFacade(agent)

	ctx, cancel := context.WithCancel(t.Context())
	ch, err := f.SendCompletion(ctx, channels.InstanceRef{}, slackMsg("long task"))
	require.NoError(t, err)
	cancel()
	drain(t, ch)

	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return len(agent.canceled) == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, []a2apkg.TaskID{taskInfo.TaskID}, agent.canceled)
	require.Equal(t, []string{"inst-kagent/worker-1"}, agent.canceledOn)
	require.NotNil(t, agent.streamCtx)
}

// smoke test that the compile-time interface assertions hold.
var _ store.Store = memory.New()

func TestFacade_SessionResumable(t *testing.T) {
	msg := slackMsg("still there?")
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", Agent: "kagent/worker"}

	t.Run("no binding: starting fresh", func(t *testing.T) {
		f, _ := newA2AFacade(newFakeAgent())
		exists, checked := f.SessionResumable(t.Context(), msg)
		require.True(t, checked)
		require.False(t, exists)
	})

	t.Run("bound to a live instance", func(t *testing.T) {
		agent := newFakeAgent()
		agent.instances["inst-1"] = pkga2a.Instance{ID: "inst-1", State: "AGENT_INSTANCE_STATE_SUSPENDED"}
		f, routes := newA2AFacade(agent)
		require.NoError(t, routes.Put(t.Context(), key, store.Entry{AgentInstanceID: "inst-1"}))
		exists, checked := f.SessionResumable(t.Context(), msg)
		require.True(t, checked)
		require.True(t, exists)
	})

	t.Run("bound to a deleted instance drops the binding", func(t *testing.T) {
		f, routes := newA2AFacade(newFakeAgent())
		require.NoError(t, routes.Put(t.Context(), key, store.Entry{AgentInstanceID: "inst-gone"}))
		exists, checked := f.SessionResumable(t.Context(), msg)
		require.True(t, checked)
		require.False(t, exists)
		_, ok, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.False(t, ok, "the next turn must create a fresh instance")
	})

	t.Run("lookup error is indeterminate", func(t *testing.T) {
		agent := newFakeAgent()
		agent.getErr = errors.New("boom")
		f, routes := newA2AFacade(agent)
		require.NoError(t, routes.Put(t.Context(), key, store.Entry{AgentInstanceID: "inst-1"}))
		_, checked := f.SessionResumable(t.Context(), msg)
		require.False(t, checked)
	})

	t.Run("no agent client configured", func(t *testing.T) {
		_, checked := (&channels.Facade{}).SessionResumable(t.Context(), msg)
		require.False(t, checked)
	})
}

func TestFacade_ResetSession(t *testing.T) {
	msg := slackMsg("resend")
	key := store.Key{Channel: "slack", ChannelID: "C1", ThreadID: "1700.0001", Agent: "kagent/worker"}

	t.Run("deletes the instance and the binding", func(t *testing.T) {
		agent := newFakeAgent()
		agent.instances["inst-1"] = pkga2a.Instance{ID: "inst-1"}
		f, routes := newA2AFacade(agent)
		require.NoError(t, routes.Put(t.Context(), key, store.Entry{AgentInstanceID: "inst-1"}))
		reset, err := f.ResetSession(t.Context(), msg)
		require.NoError(t, err)
		require.True(t, reset)
		require.Equal(t, []string{"inst-1"}, agent.deleted)
		_, ok, err := routes.Get(t.Context(), key)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("delete failure is reported", func(t *testing.T) {
		agent := newFakeAgent()
		agent.deleteErr = errors.New("boom")
		f, routes := newA2AFacade(agent)
		require.NoError(t, routes.Put(t.Context(), key, store.Entry{AgentInstanceID: "inst-1"}))
		reset, err := f.ResetSession(t.Context(), msg)
		require.Error(t, err)
		require.False(t, reset)
	})

	t.Run("no binding: nothing to reset", func(t *testing.T) {
		f, _ := newA2AFacade(newFakeAgent())
		reset, err := f.ResetSession(t.Context(), msg)
		require.NoError(t, err)
		require.False(t, reset)
	})

	t.Run("no agent client configured", func(t *testing.T) {
		reset, err := (&channels.Facade{}).ResetSession(t.Context(), msg)
		require.NoError(t, err)
		require.False(t, reset)
	})
}

func TestFacade_ListAgents(t *testing.T) {
	agent := newFakeAgent()
	agent.agents = []pkga2a.AgentInfo{{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent"}}
	f, _ := newA2AFacade(agent)
	agents, err := f.ListAgents(t.Context())
	require.NoError(t, err)
	require.Equal(t, agent.agents, agents)

	_, err = (&channels.Facade{}).ListAgents(t.Context())
	require.Error(t, err)
}
