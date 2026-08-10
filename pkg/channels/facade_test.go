package channels_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"strings"
	"testing"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
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

// fakeChannelExecutor records calls and emits a fixed event sequence.
type fakeChannelExecutor struct {
	events []a2apkg.Event
	err    error
	gotCtx context.Context
}

func (e *fakeChannelExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		e.gotCtx = ctx
		if e.err != nil {
			yield(nil, e.err)
			return
		}
		for _, ev := range e.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func TestFacade_SendCompletionViaA2A_ArtifactEvents(t *testing.T) {
	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx",
		TaskID:    a2apkg.NewTaskID(),
		Message:   a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("hi")),
	}
	artifact := a2apkg.NewArtifactEvent(execCtx, a2apkg.NewTextPart("hello world"))
	terminal := a2apkg.NewStatusUpdateEvent(execCtx, a2apkg.TaskStateCompleted, nil)

	exec := &fakeChannelExecutor{events: []a2apkg.Event{artifact, terminal}}
	f := &channels.Facade{Executor: exec}

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, channels.InboundMessage{
		Channel:  "slack",
		AgentRef: "worker",
		Text:     "hi",
	})
	require.NoError(t, err)

	var content strings.Builder
	var done bool
	for d := range ch {
		require.NoError(t, d.Err)
		if d.Done {
			done = true
		}
		content.WriteString(d.Content)
	}
	require.Equal(t, "hello world", content.String())
	require.True(t, done)
}

// kagentPart builds a kagent function_call/function_response DataPart the way
// the wire format spells it.
func kagentPart(kagentType, name, id string) *a2apkg.Part {
	p := a2apkg.NewDataPart(map[string]any{"name": name, "id": id})
	p.Metadata = map[string]any{"kagent_type": kagentType}
	return p
}

// The issue's turn: three rounds of "narrate, then call tools", then the final
// answer, which kagent sends twice — as a text-only working event and as the
// artifact. Every narration must arrive exactly once, and the answer must not be
// duplicated (klaus-gateway#197).
func TestFacade_SendCompletionViaA2A_NarrationDeliveredOnceWithFinalAnswer(t *testing.T) {
	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx",
		TaskID:    a2apkg.NewTaskID(),
		Message:   a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("compare both clusters")),
	}
	const (
		narration1 = "Let me pull the HelmRelease from both clusters simultaneously."
		narration2 = "Both HelmReleases share the same chart version — the differences will be in the ConfigMaps."
		answer     = "Here is the focused diff on the klausGateway section:"
	)
	working := func(parts ...*a2apkg.Part) a2apkg.Event {
		msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, parts...)
		return a2apkg.NewStatusUpdateEvent(execCtx, a2apkg.TaskStateWorking, msg)
	}
	call := func(id string) *a2apkg.Part { return kagentPart("function_call", "kubectl_get", id) }
	resp := func(id string) *a2apkg.Part { return kagentPart("function_response", "kubectl_get", id) }

	exec := &fakeChannelExecutor{events: []a2apkg.Event{
		// kagent echoes the user's own message back as the submitted event.
		a2apkg.NewStatusUpdateEvent(execCtx, a2apkg.TaskStateSubmitted, execCtx.Message),
		working(a2apkg.NewTextPart(narration1), call("c1"), call("c2")),
		working(resp("c1"), resp("c2")),
		working(a2apkg.NewTextPart(narration2), call("c3")),
		working(resp("c3")),
		working(a2apkg.NewTextPart(answer)), // the mirror of the final answer
		a2apkg.NewArtifactEvent(execCtx, a2apkg.NewTextPart(answer)),
		a2apkg.NewStatusUpdateEvent(execCtx, a2apkg.TaskStateCompleted, nil),
	}}
	f := &channels.Facade{Executor: exec}

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, channels.InboundMessage{
		Channel:  "slack",
		AgentRef: "worker",
		Text:     "compare both clusters",
	})
	require.NoError(t, err)

	var narrations, texts []string
	var tools int
	var done bool
	for d := range ch {
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

func TestFacade_SendCompletionViaA2A_ForwardsBearerToken(t *testing.T) {
	terminal := a2apkg.NewStatusUpdateEvent(&a2asrv.ExecutorContext{
		ContextID: "ctx",
		TaskID:    a2apkg.NewTaskID(),
		Message:   a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("hi")),
	}, a2apkg.TaskStateCompleted, nil)

	exec := &fakeChannelExecutor{events: []a2apkg.Event{terminal}}
	f := &channels.Facade{Executor: exec}

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, channels.InboundMessage{
		Channel:     "web",
		AgentRef:    "worker",
		Text:        "hi",
		BearerToken: "user-jwt",
	})
	require.NoError(t, err)
	for range ch { //nolint:revive // drain to ensure Execute ran
	}

	require.Equal(t, "worker", pkga2a.AgentRefFromContext(exec.gotCtx))
	require.Equal(t, "user-jwt", pkga2a.ForwardedTokenFromContext(exec.gotCtx))
}

func TestFacade_SendCompletionViaA2A_ErrorPropagated(t *testing.T) {
	exec := &fakeChannelExecutor{err: errors.New("executor boom")}
	f := &channels.Facade{Executor: exec}

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, channels.InboundMessage{
		AgentRef: "worker",
		Text:     "hi",
	})
	require.NoError(t, err)

	var gotErr error
	for d := range ch {
		if d.Err != nil {
			gotErr = d.Err
		}
	}
	require.ErrorContains(t, gotErr, "executor boom")
}

func TestFacade_SendCompletionFallsBackToOpenAI_WhenNoAgentRef(t *testing.T) {
	exec := &fakeChannelExecutor{}
	client := &fakeClient{sseBody: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"}
	f := &channels.Facade{Executor: exec, Client: client}

	// AgentRef is empty, so the OpenAI path must be used even though Executor is set.
	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{Name: "i1"}, channels.InboundMessage{Text: "hi"})
	require.NoError(t, err)

	var content strings.Builder
	for d := range ch {
		content.WriteString(d.Content)
	}
	require.Equal(t, "ok", content.String())
}

func TestFacade_SendCompletionViaA2A_InputRequired_EmitsPromptDelta(t *testing.T) {
	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx",
		TaskID:    a2apkg.NewTaskID(),
		Message:   a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("hi")),
	}
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("approve the tool call?"))
	inputRequired := a2apkg.NewStatusUpdateEvent(execCtx, a2apkg.TaskStateInputRequired, msg)

	exec := &fakeChannelExecutor{events: []a2apkg.Event{inputRequired}}
	f := &channels.Facade{Executor: exec}

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, channels.InboundMessage{
		AgentRef: "worker",
		Text:     "hi",
	})
	require.NoError(t, err)

	var deltas []channels.OutboundDelta
	for d := range ch {
		deltas = append(deltas, d)
	}
	require.Len(t, deltas, 1)
	require.Equal(t, channels.DeltaPrompt, deltas[0].Kind)
	require.Equal(t, "approve the tool call?", deltas[0].Content)
}

func TestFacade_SendCompletionViaA2A_AuthRequired_EmitsPromptDelta(t *testing.T) {
	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx",
		TaskID:    a2apkg.NewTaskID(),
		Message:   a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart("hi")),
	}
	authRequired := a2apkg.NewStatusUpdateEvent(execCtx, a2apkg.TaskStateAuthRequired, nil)

	exec := &fakeChannelExecutor{events: []a2apkg.Event{authRequired}}
	f := &channels.Facade{Executor: exec}

	ch, err := f.SendCompletion(t.Context(), channels.InstanceRef{}, channels.InboundMessage{
		AgentRef: "worker",
		Text:     "hi",
	})
	require.NoError(t, err)

	var deltas []channels.OutboundDelta
	for d := range ch {
		deltas = append(deltas, d)
	}
	require.Len(t, deltas, 1)
	require.Equal(t, channels.DeltaPrompt, deltas[0].Kind)
}

// smoke test that the compile-time interface assertions hold.
var _ store.Store = memory.New()

type fakeSessions struct {
	exists bool
	err    error
	gotID  string
}

func (f *fakeSessions) Exists(_ context.Context, id string) (bool, error) {
	f.gotID = id
	return f.exists, f.err
}

func TestFacade_SessionResumable(t *testing.T) {
	msg := channels.InboundMessage{Channel: "slack", ChannelID: "C1", ThreadID: "T1", AgentRef: "sre"}
	want := channels.SynthesizeContextID(msg.Channel, msg.ChannelID, msg.UserID, msg.ThreadID, msg.AgentRef)

	fs := &fakeSessions{exists: true}
	f := &channels.Facade{Sessions: fs}
	exists, checked := f.SessionResumable(context.Background(), msg)
	require.True(t, checked)
	require.True(t, exists)
	require.Equal(t, want, fs.gotID, "the synthesized contextID is looked up")

	// A lookup error is reported as indeterminate (checked=false).
	f = &channels.Facade{Sessions: &fakeSessions{err: errors.New("boom")}}
	exists, checked = f.SessionResumable(context.Background(), msg)
	require.False(t, checked)
	require.False(t, exists)

	// No session client configured -> unavailable.
	_, checked = (&channels.Facade{}).SessionResumable(context.Background(), msg)
	require.False(t, checked)
}

type fakeSessionsDeleter struct {
	fakeSessions
	deleteErr error
	deletedID string
}

func (f *fakeSessionsDeleter) Delete(_ context.Context, id string) error {
	f.deletedID = id
	return f.deleteErr
}

func TestFacade_ResetSession(t *testing.T) {
	msg := channels.InboundMessage{Channel: "slack", ChannelID: "C1", ThreadID: "T1", AgentRef: "sre"}
	want := channels.SynthesizeContextID(msg.Channel, msg.ChannelID, msg.UserID, msg.ThreadID, msg.AgentRef)

	fs := &fakeSessionsDeleter{}
	f := &channels.Facade{Sessions: fs}
	reset, err := f.ResetSession(context.Background(), msg)
	require.NoError(t, err)
	require.True(t, reset)
	require.Equal(t, want, fs.deletedID, "the synthesized contextID is deleted")

	// A delete failure is reported so the caller can degrade its notice.
	f = &channels.Facade{Sessions: &fakeSessionsDeleter{deleteErr: errors.New("boom")}}
	reset, err = f.ResetSession(context.Background(), msg)
	require.Error(t, err)
	require.False(t, reset)

	// A session client without Delete -> unavailable, not an error.
	f = &channels.Facade{Sessions: &fakeSessions{}}
	reset, err = f.ResetSession(context.Background(), msg)
	require.NoError(t, err)
	require.False(t, reset)

	// No session client configured -> unavailable.
	reset, err = (&channels.Facade{}).ResetSession(context.Background(), msg)
	require.NoError(t, err)
	require.False(t, reset)
}
