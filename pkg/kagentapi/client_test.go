package kagentapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/kagentapi"
)

func TestClient_PushEvent(t *testing.T) {
	var (
		gotURL    string
		gotBearer string
		gotUserID string
		gotAgent  string
		gotBody   []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		gotBearer = r.Header.Get("Authorization")
		gotUserID = r.Header.Get("X-User-Id")
		gotAgent = r.Header.Get("X-Agent-Name")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := kagentapi.New(srv.URL, "test-agent")
	require.True(t, client.Enabled())

	auth := kagentapi.AuthInfo{BearerToken: "tok123", UserSub: "alice"}
	event := kagentapi.NewSessionEvent("ev-1", "user", "hello")
	client.PushEvent(t.Context(), "sess-abc", event, auth)

	require.Equal(t, "/api/sessions/sess-abc/events", gotURL)
	require.Equal(t, "Bearer tok123", gotBearer)
	require.Equal(t, "alice", gotUserID)
	require.Equal(t, "test-agent", gotAgent)

	var got kagentapi.SessionEvent
	require.NoError(t, json.Unmarshal(gotBody, &got))
	require.Equal(t, "ev-1", got.ID)
}

func TestClient_StoreTask(t *testing.T) {
	var (
		gotURL  string
		gotBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := kagentapi.New(srv.URL, "agent-ref")
	auth := kagentapi.AuthInfo{BearerToken: "tok", UserSub: "bob"}
	client.StoreTask(t.Context(), "task-1", "ctx-1", "user text", "agent text", auth)

	require.Equal(t, "/api/tasks", gotURL)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	require.Equal(t, "task-1", payload["id"])
	require.Equal(t, "ctx-1", payload["contextId"])
	status, ok := payload["status"].(map[string]any)
	require.True(t, ok, "status must be a map")
	require.Equal(t, "completed", status["state"])
}

func TestClient_DisabledWhenNoEndpoint(t *testing.T) {
	client := kagentapi.New("", "agent-ref")
	require.False(t, client.Enabled())

	// No panic, no request sent.
	client.PushEvent(t.Context(), "sess", kagentapi.NewSessionEvent("ev", "user", "hi"), kagentapi.AuthInfo{})
	client.StoreTask(t.Context(), "task", "ctx", "u", "a", kagentapi.AuthInfo{})
}
