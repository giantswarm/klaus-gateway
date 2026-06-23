package a2a_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// startFakeKagent starts an httptest.Server that responds to "message/stream" JSON-RPC
// with a kagent-format SSE stream: submitted → artifact("pong") → completed.
func startFakeKagent(t *testing.T, agentName string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/a2a/kagent/"+agentName+"/a2a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		contextID := "ctx-fake"
		taskID := "task-fake"
		writeSSEResult(w, flusher, map[string]any{
			"kind":      "status-update",
			"contextId": contextID,
			"taskId":    taskID,
			"final":     false,
			"status":    map[string]any{"state": "submitted"},
		})
		writeSSEResult(w, flusher, map[string]any{
			"kind":      "artifact-update",
			"contextId": contextID,
			"taskId":    taskID,
			"lastChunk": true,
			"artifact": map[string]any{
				"artifactId": "art-fake",
				"parts":      []any{map[string]any{"kind": "text", "text": "pong"}},
			},
		})
		writeSSEResult(w, flusher, map[string]any{
			"kind":      "status-update",
			"contextId": contextID,
			"taskId":    taskID,
			"final":     true,
			"status":    map[string]any{"state": "completed"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeSSEResult(w http.ResponseWriter, flusher http.Flusher, result any) {
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "1", "result": result})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// TestKagentClient_Execute_SerializesDataPart verifies a structured HITL
// decision (DataPart) is forwarded as {"kind":"data","data":{...}} alongside
// the text label. kagent resolves the paused confirmation only from a DataPart.
func TestKagentClient_Execute_SerializesDataPart(t *testing.T) {
	const agentName = "klaud-hitl"

	var gotParts []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/a2a/kagent/"+agentName+"/a2a", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Message struct {
					Parts []map[string]any `json:"parts"`
				} `json:"message"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotParts = body.Params.Message.Parts

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeSSEResult(w, flusher, map[string]any{
			"kind": "status-update", "taskId": "t", "final": true,
			"status": map[string]any{"state": "completed"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	kc := &pkga2a.A2AClient{BaseURL: srv.URL + "/api/a2a/kagent", DefaultAgent: agentName}

	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx",
		TaskID:    a2a.NewTaskID(),
		Message: a2a.NewMessage(a2a.MessageRoleUser,
			a2a.NewDataPart(map[string]any{"decision_type": "approve"}),
			a2a.NewTextPart("approved"),
		),
	}
	for _, err := range kc.Execute(t.Context(), execCtx) {
		require.NoError(t, err)
	}

	require.Len(t, gotParts, 2)
	require.Equal(t, "data", gotParts[0]["kind"])
	data, ok := gotParts[0]["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "approve", data["decision_type"])
	require.Equal(t, "text", gotParts[1]["kind"])
	require.Equal(t, "approved", gotParts[1]["text"])
}

func TestKagentClient_Execute_ForwardsEvents(t *testing.T) {
	const agentName = "klaud-coding"
	srv := startFakeKagent(t, agentName)

	kc := &pkga2a.A2AClient{
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
	require.True(t, ok, "last event must be a TaskStatusUpdateEvent, got %T", last)
	require.True(t, terminal.Status.State.Terminal())

	// Verify artifact event is in the stream.
	var artifactSeen bool
	for _, ev := range events {
		if art, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
			artifactSeen = true
			require.Equal(t, "pong", art.Artifact.Parts[0].Text())
		}
	}
	require.True(t, artifactSeen, "expected at least one artifact event")
}

func TestKagentClient_Execute_UsesAgentRefFromContext(t *testing.T) {
	const agentName = "klaud-home"
	srv := startFakeKagent(t, agentName)

	kc := &pkga2a.A2AClient{
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

func TestKagentClient_Execute_ForwardsBearerToken(t *testing.T) {
	const agentName = "klaud-coding"

	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/a2a/kagent/"+agentName+"/a2a", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		writeSSEResult(w, flusher, map[string]any{
			"kind": "status-update", "contextId": "c", "taskId": "t",
			"final": true, "status": map[string]any{"state": "completed"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	kc := &pkga2a.A2AClient{
		BaseURL:      srv.URL + "/api/a2a/kagent",
		DefaultAgent: agentName,
		TokenSource:  pkga2a.ForwardedTokenSource{Fallback: nil},
	}

	ctx := pkga2a.WithForwardedToken(t.Context(), "user-jwt")
	execCtx := &a2asrv.ExecutorContext{
		ContextID: "ctx-tok",
		TaskID:    a2a.NewTaskID(),
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
	}
	for _, err := range kc.Execute(ctx, execCtx) {
		require.NoError(t, err)
	}

	require.Equal(t, "Bearer user-jwt", gotAuth)
}

func TestAgentRefContext_RoundTrip(t *testing.T) {
	ctx := pkga2a.WithAgentRef(context.Background(), "my-agent")
	require.Equal(t, "my-agent", pkga2a.AgentRefFromContext(ctx))
	require.Empty(t, pkga2a.AgentRefFromContext(context.Background()))
}
