package a2a_test

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// echoKagentExecutor emits a single artifact and a completed event.
type echoKagentExecutor struct{}

func (e *echoKagentExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		artifact := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart("pong"))
		artifact.LastChunk = true
		if !yield(artifact, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *echoKagentExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func startFakeKagent(t *testing.T, agentName string) *httptest.Server {
	t.Helper()
	handler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(&echoKagentExecutor{}))
	mux := http.NewServeMux()
	// client.go appends /a2a to the base URL before dialling
	mux.Handle("/api/a2a/kagent/"+agentName+"/a2a", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestKagentClient_Execute_ForwardsEvents(t *testing.T) {
	const agentName = "klaud-coding"
	srv := startFakeKagent(t, agentName)

	kc := &pkga2a.A2AClient{
		Clients:      pkga2a.NewClients(""),
		BaseURL:      srv.URL + "/api/a2a/kagent",
		DefaultAgent: agentName,
	}

	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx-test",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
	}

	var events []a2a.Event
	for event, err := range kc.Execute(t.Context(), execCtx) {
		require.NoError(t, err)
		events = append(events, event)
	}

	require.NotEmpty(t, events)
	last := events[len(events)-1]
	terminal, ok := last.(*a2a.TaskStatusUpdateEvent)
	require.True(t, ok, "last event must be a TaskStatusUpdateEvent")
	require.True(t, terminal.Status.State.Terminal())
}

func TestKagentClient_Execute_UsesAgentRefFromContext(t *testing.T) {
	const agentName = "klaud-home"
	srv := startFakeKagent(t, agentName)

	kc := &pkga2a.A2AClient{
		Clients:      pkga2a.NewClients(""),
		BaseURL:      srv.URL + "/api/a2a/kagent",
		DefaultAgent: "wrong-default",
	}

	ctx := pkga2a.WithAgentRef(t.Context(), agentName)
	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx-ref",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")),
	}

	var events []a2a.Event
	for event, err := range kc.Execute(ctx, execCtx) {
		require.NoError(t, err)
		events = append(events, event)
	}
	require.NotEmpty(t, events)
}

func TestAgentRefContext_RoundTrip(t *testing.T) {
	ctx := pkga2a.WithAgentRef(t.Context(), "my-agent")
	require.Equal(t, "my-agent", pkga2a.AgentRefFromContext(ctx))
	require.Empty(t, pkga2a.AgentRefFromContext(t.Context()))
}
