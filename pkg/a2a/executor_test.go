package a2a_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	a2aclient "github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/kagentapi"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
)

// collectingQueue collects events written by the executor for assertion.
type collectingQueue struct {
	mu     sync.Mutex
	events []a2a.Event
}

func (q *collectingQueue) Write(_ context.Context, event a2a.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, event)
	return nil
}

func (q *collectingQueue) WriteVersioned(ctx context.Context, event a2a.Event, _ a2a.TaskVersion) error {
	return q.Write(ctx, event)
}

func (q *collectingQueue) Read(_ context.Context) (a2a.Event, a2a.TaskVersion, error) {
	return nil, a2a.TaskVersionMissing, fmt.Errorf("not implemented")
}

func (q *collectingQueue) Close() error { return nil }

func (q *collectingQueue) snapshot() []a2a.Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	snap := make([]a2a.Event, len(q.events))
	copy(snap, q.events)
	return snap
}

// staticResolver always resolves to the given base URL.
type staticResolver struct{ baseURL string }

func (r *staticResolver) Resolve(_ context.Context, _ routing.InboundMessage) (lifecycle.InstanceRef, error) {
	return lifecycle.InstanceRef{BaseURL: r.baseURL}, nil
}

// dialerFunc wraps a function to satisfy the Dialer interface.
type dialerFunc func(ctx context.Context, baseURL string) (*a2aclient.Client, error)

func (f dialerFunc) For(ctx context.Context, baseURL string) (*a2aclient.Client, error) {
	return f(ctx, baseURL)
}

// recordingPusher records kagent push calls.
type recordingPusher struct {
	mu     sync.Mutex
	events []kagentapi.SessionEvent
	tasks  []string
}

func (p *recordingPusher) Enabled() bool { return true }

func (p *recordingPusher) PushEvent(_ context.Context, _ string, event kagentapi.SessionEvent, _ kagentapi.AuthInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
}

func (p *recordingPusher) StoreTask(_ context.Context, taskID, _, _, _ string, _ kagentapi.AuthInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasks = append(p.tasks, taskID)
}

// fakePodExecutor emits a fixed event sequence that ends with a Final event.
type fakePodExecutor struct{}

func (e *fakePodExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	artifact := a2a.NewArtifactEvent(reqCtx, a2a.TextPart{Text: "hello world"})
	_ = queue.Write(ctx, artifact)
	final := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCompleted, nil)
	final.Final = true
	_ = queue.Write(ctx, final)
	return nil
}

func (e *fakePodExecutor) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCanceled, nil)
	ev.Final = true
	return queue.Write(ctx, ev)
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

	queue := &collectingQueue{}
	reqCtx := &a2asrv.RequestContext{
		ContextID: "ctx-test-1",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "ping"}),
	}

	err := executor.Execute(t.Context(), reqCtx, queue)
	require.NoError(t, err)

	events := queue.snapshot()
	// Expect at least: working (initial) + artifact + completed.
	require.GreaterOrEqual(t, len(events), 3, "expected working + artifact + completed events")

	// First event must be a working status.
	working, ok := events[0].(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok, "first event must be *TaskStatusUpdateEvent")
	require.Equal(t, a2a.TaskStateWorking, working.Status.State)

	// Last event must be completed.
	last := events[len(events)-1]
	completed, ok := last.(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok, "last event must be *TaskStatusUpdateEvent")
	require.Equal(t, a2a.TaskStateCompleted, completed.Status.State)
	require.True(t, completed.Final)

	// All events carry the gateway contextID, not the pod contextID.
	for _, ev := range events {
		switch e := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			require.Equal(t, reqCtx.ContextID, e.ContextID)
		case *a2a.TaskArtifactUpdateEvent:
			require.Equal(t, reqCtx.ContextID, e.ContextID)
		}
	}

	// Kagent push is async; wait briefly.
	require.Eventually(t, func() bool {
		pusher.mu.Lock()
		defer pusher.mu.Unlock()
		return len(pusher.tasks) > 0
	}, 2*time.Second, 50*time.Millisecond, "kagent StoreTask not called")
}

func TestForwardingExecutor_Execute_RouteNotFound(t *testing.T) {
	executor := &pkga2a.ForwardingExecutor{
		Router: &failingResolver{err: fmt.Errorf("no route")},
		Dial:   pkga2a.NewClients(),
		Kagent: &recordingPusher{},
	}

	queue := &collectingQueue{}
	reqCtx := &a2asrv.RequestContext{
		ContextID: "missing-ctx",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hello"}),
	}

	err := executor.Execute(t.Context(), reqCtx, queue)
	require.NoError(t, err)

	events := queue.snapshot()
	require.Len(t, events, 2, "expected working + failed events")
	working, ok := events[0].(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateWorking, working.Status.State)
	failed, ok := events[1].(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateFailed, failed.Status.State)
	require.True(t, failed.Final)
}

type failingResolver struct{ err error }

func (r *failingResolver) Resolve(_ context.Context, _ routing.InboundMessage) (lifecycle.InstanceRef, error) {
	return lifecycle.InstanceRef{}, r.err
}

// failingPodExecutor emits a Final failed event.
type failingPodExecutor struct{ msg string }

func (e *failingPodExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateFailed, &a2a.Message{
		Role:  a2a.MessageRoleAgent,
		Parts: []a2a.Part{a2a.TextPart{Text: e.msg}},
	})
	ev.Final = true
	return queue.Write(ctx, ev)
}

func (e *failingPodExecutor) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCanceled, nil)
	ev.Final = true
	return queue.Write(ctx, ev)
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

	queue := &collectingQueue{}
	reqCtx := &a2asrv.RequestContext{
		ContextID: "ctx-fail",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "do it"}),
	}

	err := executor.Execute(t.Context(), reqCtx, queue)
	require.NoError(t, err)

	events := queue.snapshot()
	last := events[len(events)-1]
	failed, ok := last.(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok, "last event must be *TaskStatusUpdateEvent")
	require.Equal(t, a2a.TaskStateFailed, failed.Status.State)
	require.True(t, failed.Final)
}
