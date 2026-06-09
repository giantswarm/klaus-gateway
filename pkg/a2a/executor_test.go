package a2a_test

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/kagentapi"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/static"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
)

// collectEvents drains an event iterator, returning all events or the first error.
func collectEvents(ctx context.Context, seq iter.Seq2[a2a.Event, error]) ([]a2a.Event, error) {
	var events []a2a.Event
	for event, err := range seq {
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	_ = ctx
	return events, nil
}

// staticResolver always resolves to the given base URL.
type staticResolver struct{ baseURL string }

func (r *staticResolver) Resolve(_ context.Context, _ routing.InboundMessage) (lifecycle.InstanceRef, error) {
	return lifecycle.InstanceRef{BaseURL: r.baseURL}, nil
}

// recordingPusher records kagent push calls.
type recordingPusher struct {
	mu           sync.Mutex
	events       []kagentapi.SessionEvent
	tasks        []string
	agentMetaAll []map[string]any
}

func (p *recordingPusher) Enabled() bool { return true }

func (p *recordingPusher) PushEvent(_ context.Context, _ string, event kagentapi.SessionEvent, _ kagentapi.AuthInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
}

func (p *recordingPusher) StoreTask(_ context.Context, taskID, _, _, _, _ string, agentMetadata map[string]any, _ kagentapi.AuthInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasks = append(p.tasks, taskID)
	p.agentMetaAll = append(p.agentMetaAll, agentMetadata)
}

// fakePodExecutor emits a fixed event sequence that ends with a terminal event.
type fakePodExecutor struct{}

func (e *fakePodExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		artifact := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart("hello world"))
		artifact.LastChunk = true
		if !yield(artifact, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *fakePodExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// startFakePod returns an httptest server that serves an A2A endpoint at /a2a.
func startFakePod(t *testing.T) *httptest.Server {
	t.Helper()
	h := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(&fakePodExecutor{}))
	mux := http.NewServeMux()
	mux.Handle("/a2a", h)
	mux.Handle("/a2a/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestForwardingExecutor_Execute_RelaysEvents(t *testing.T) {
	pod := startFakePod(t)
	pusher := &recordingPusher{}

	executor := &pkga2a.ForwardingExecutor{
		Router: &staticResolver{baseURL: pod.URL},
		Dial:   pkga2a.NewClients(),
		Kagent: pusher,
	}

	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx-test-1",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
	}

	events, err := collectEvents(t.Context(), executor.Execute(t.Context(), execCtx))
	require.NoError(t, err)
	// Expect at least: submitted + working (initial) + artifact + completed.
	require.GreaterOrEqual(t, len(events), 4, "expected submitted + working + artifact + completed events")

	// First event must be the submitted task.
	_, ok := events[0].(*a2a.Task)
	require.True(t, ok, "first event must be *Task")

	// Second event must be a working status.
	working, ok := events[1].(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok, "second event must be *TaskStatusUpdateEvent")
	require.Equal(t, a2a.TaskStateWorking, working.Status.State)

	// Last event must be completed.
	last := events[len(events)-1]
	completed, ok := last.(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok, "last event must be *TaskStatusUpdateEvent")
	require.Equal(t, a2a.TaskStateCompleted, completed.Status.State)
	require.True(t, completed.Status.State.Terminal())

	// Exactly one artifact event must have LastChunk=true and it must appear
	// before the completed event (the kagent UI only commits streaming text on
	// receipt of lastChunk, so it must arrive before the terminal event).
	var lastChunkIdx int
	lastChunkCount := 0
	for i, ev := range events {
		if ae, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok && ae.LastChunk {
			lastChunkIdx = i
			lastChunkCount++
		}
	}
	require.Equal(t, 1, lastChunkCount, "expected exactly one artifact event with LastChunk=true")
	require.Less(t, lastChunkIdx, len(events)-1, "lastChunk artifact must precede the completed event")

	// All events carry the gateway contextID, not the pod contextID.
	for _, ev := range events {
		switch e := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			require.Equal(t, execCtx.ContextID, e.ContextID)
		case *a2a.TaskArtifactUpdateEvent:
			require.Equal(t, execCtx.ContextID, e.ContextID)
		}
	}

	// StoreTask is now synchronous; it must be called before Execute returns.
	pusher.mu.Lock()
	taskCount := len(pusher.tasks)
	pusher.mu.Unlock()
	require.Greater(t, taskCount, 0, "kagent StoreTask not called")
}

func TestForwardingExecutor_Execute_RouteNotFound(t *testing.T) {
	executor := &pkga2a.ForwardingExecutor{
		Router: &failingResolver{err: fmt.Errorf("no route")},
		Dial:   pkga2a.NewClients(),
		Kagent: &recordingPusher{},
	}

	execCtx := &a2asrv.ExecutorContext{
		ContextID: "missing-ctx",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")),
	}

	events, err := collectEvents(t.Context(), executor.Execute(t.Context(), execCtx))
	require.NoError(t, err)
	require.Len(t, events, 3, "expected submitted + working + failed events")
	_, ok := events[0].(*a2a.Task)
	require.True(t, ok, "first event must be *Task")
	working, ok := events[1].(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateWorking, working.Status.State)
	failed, ok := events[2].(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateFailed, failed.Status.State)
	require.True(t, failed.Status.State.Terminal())
}

type failingResolver struct{ err error }

func (r *failingResolver) Resolve(_ context.Context, _ routing.InboundMessage) (lifecycle.InstanceRef, error) {
	return lifecycle.InstanceRef{}, r.err
}

// failingPodExecutor emits a terminal failed event.
type failingPodExecutor struct{ msg string }

func (e *failingPodExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed,
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(e.msg))), nil)
	}
}

func (e *failingPodExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func startPodWith(t *testing.T, exec a2asrv.AgentExecutor) *httptest.Server {
	t.Helper()
	h := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec))
	mux := http.NewServeMux()
	mux.Handle("/a2a", h)
	mux.Handle("/a2a/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestForwardingExecutor_Execute_PodFailurePropagated(t *testing.T) {
	pod := startPodWith(t, &failingPodExecutor{msg: "something went wrong"})

	executor := &pkga2a.ForwardingExecutor{
		Router: &staticResolver{baseURL: pod.URL},
		Dial:   pkga2a.NewClients(),
		Kagent: &recordingPusher{},
	}

	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx-fail",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("do it")),
	}

	events, err := collectEvents(t.Context(), executor.Execute(t.Context(), execCtx))
	require.NoError(t, err)
	last := events[len(events)-1]
	failed, ok := last.(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok, "last event must be *TaskStatusUpdateEvent")
	require.Equal(t, a2a.TaskStateFailed, failed.Status.State)
	require.True(t, failed.Status.State.Terminal())
}

// tokenUsagePodExecutor emits an artifact with token_usage metadata (as Klaus does).
type tokenUsagePodExecutor struct{}

func (e *tokenUsagePodExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		artifact := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart("forty-two"))
		artifact.LastChunk = true
		// Simulate what Klaus sets: generic token_usage, integer values.
		artifact.Artifact.Metadata = map[string]any{
			"token_usage": map[string]any{
				"input_tokens":  float64(100),
				"output_tokens": float64(42),
			},
		}
		if !yield(artifact, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *tokenUsagePodExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func TestForwardingExecutor_Execute_KagentUsageMetadata(t *testing.T) {
	pod := startPodWith(t, &tokenUsagePodExecutor{})

	pusher := &recordingPusher{}
	executor := &pkga2a.ForwardingExecutor{
		Router: &staticResolver{baseURL: pod.URL},
		Dial:   pkga2a.NewClients(),
		Kagent: pusher,
	}

	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx-usage",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("what is 6x7?")),
	}

	_, err := collectEvents(t.Context(), executor.Execute(t.Context(), execCtx))
	require.NoError(t, err)

	pusher.mu.Lock()
	defer pusher.mu.Unlock()

	require.NotEmpty(t, pusher.agentMetaAll, "StoreTask must be called")
	agentMeta := pusher.agentMetaAll[0]
	require.NotNil(t, agentMeta, "agent metadata must be non-nil when pod emits token_usage")

	kagentMeta, ok := agentMeta["kagent_usage_metadata"].(map[string]any)
	require.True(t, ok, "kagent_usage_metadata must be a map[string]any")
	require.EqualValues(t, int64(100), kagentMeta["promptTokenCount"])
	require.EqualValues(t, int64(42), kagentMeta["candidatesTokenCount"])
	require.EqualValues(t, int64(142), kagentMeta["totalTokenCount"])
}

// echoExecutor emits a single artifact containing the fixed text string.
type echoExecutor struct{ text string }

func (e *echoExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		artifact := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(e.text))
		artifact.LastChunk = true
		if !yield(artifact, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *echoExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// extractArtifactText returns the concatenated text of the first artifact event.
func extractArtifactText(events []a2a.Event) string {
	for _, ev := range events {
		if ae, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok && ae.Artifact != nil {
			var sb strings.Builder
			for _, part := range ae.Artifact.Parts {
				if part != nil {
					sb.WriteString(part.Text())
				}
			}
			return sb.String()
		}
	}
	return ""
}

func TestForwardingExecutor_MultiAgentRouting(t *testing.T) {
	// Two pods emit distinct text so the test can tell them apart.
	podA := startPodWith(t, &echoExecutor{text: "from-A"})
	podB := startPodWith(t, &echoExecutor{text: "from-B"})

	manager, err := static.New("worker-a=" + podA.URL + ",worker-b=" + podB.URL)
	require.NoError(t, err)
	router := routing.New(memory.New(), manager, true, 24*time.Hour)

	executor := &pkga2a.ForwardingExecutor{
		Router: router,
		Dial:   pkga2a.NewClients(),
		Kagent: &recordingPusher{},
	}

	t.Run("worker-a routes to pod A", func(t *testing.T) {
		ctx := pkga2a.WithAgentRef(t.Context(), "worker-a")
		execCtx := &a2asrv.ExecutorContext{
			ContextID: "ctx-ma-a",
			TaskID:    a2a.NewTaskID(),
			Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
		}
		events, err := collectEvents(ctx, executor.Execute(ctx, execCtx))
		require.NoError(t, err)
		require.Equal(t, "from-A", extractArtifactText(events))
	})

	t.Run("worker-b routes to pod B", func(t *testing.T) {
		ctx := pkga2a.WithAgentRef(t.Context(), "worker-b")
		execCtx := &a2asrv.ExecutorContext{
			ContextID: "ctx-ma-b",
			TaskID:    a2a.NewTaskID(),
			Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
		}
		events, err := collectEvents(ctx, executor.Execute(ctx, execCtx))
		require.NoError(t, err)
		require.Equal(t, "from-B", extractArtifactText(events))
	})

	t.Run("same contextID under distinct agents routes to distinct pods", func(t *testing.T) {
		// "shared-ctx" under worker-a → pod A.
		ctxA := pkga2a.WithAgentRef(t.Context(), "worker-a")
		execCtxA := &a2asrv.ExecutorContext{
			ContextID: "shared-ctx",
			TaskID:    a2a.NewTaskID(),
			Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
		}
		eventsA, err := collectEvents(ctxA, executor.Execute(ctxA, execCtxA))
		require.NoError(t, err)
		require.Equal(t, "from-A", extractArtifactText(eventsA), "worker-a must route to pod A")

		// "shared-ctx" under worker-b → pod B (different store key).
		ctxB := pkga2a.WithAgentRef(t.Context(), "worker-b")
		execCtxB := &a2asrv.ExecutorContext{
			ContextID: "shared-ctx",
			TaskID:    a2a.NewTaskID(),
			Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
		}
		eventsB, err := collectEvents(ctxB, executor.Execute(ctxB, execCtxB))
		require.NoError(t, err)
		require.Equal(t, "from-B", extractArtifactText(eventsB), "worker-b must route to pod B, not pod A")
	})
}

func TestAgentRefMiddlewareResolution(t *testing.T) {
	// Verify that WithAgentRef/AgentRefFromContext round-trip correctly.
	ctx := pkga2a.WithAgentRef(t.Context(), "my-agent")
	require.Equal(t, "my-agent", pkga2a.AgentRefFromContext(ctx))

	// Empty context returns empty string (no panic).
	require.Empty(t, pkga2a.AgentRefFromContext(t.Context()))
}
