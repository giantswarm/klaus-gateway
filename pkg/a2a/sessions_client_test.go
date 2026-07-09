package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionsClient_Exists(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/api/sessions/present":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := &SessionsClient{
		BaseURL:     srv.URL + "/", // trailing slash must be trimmed
		TokenSource: staticToken("tok-123"),
	}

	exists, err := c.Exists(t.Context(), "present")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "/api/sessions/present", gotPath)
	require.Equal(t, "Bearer tok-123", gotAuth, "the caller's token must be forwarded")

	exists, err = c.Exists(t.Context(), "gone")
	require.NoError(t, err)
	require.False(t, exists)
}

func TestSessionsClient_UnexpectedStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := &SessionsClient{BaseURL: srv.URL}
	_, err := c.Exists(t.Context(), "x")
	require.Error(t, err)
}

type staticToken string

func (s staticToken) Token(_ context.Context) (string, error) { return string(s), nil }

func TestSessionsClient_PendingTask(t *testing.T) {
	const tasksJSON = `{"data":[
		{"id":"task-old","kind":"task","status":{"state":"completed"}},
		{"id":"task-paused","kind":"task","status":{"state":"input-required","message":{
			"role":"agent","parts":[{"kind":"data","data":{"name":"adk_request_confirmation","args":{"originalFunctionCall":{"name":"kubectl_delete","id":"call-1","args":{}}}},"metadata":{"kagent_type":"function_call","kagent_is_long_running":true}}]
		}}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/sessions/sess-1/tasks", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tasksJSON))
	}))
	defer srv.Close()

	c := &SessionsClient{BaseURL: srv.URL}
	taskID, statusMessage, err := c.PendingTask(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Equal(t, "task-paused", taskID)
	require.NotNil(t, statusMessage)
	require.Len(t, statusMessage.Parts, 1)
	data, _ := statusMessage.Parts[0].Data().(map[string]any)
	require.Equal(t, "adk_request_confirmation", data["name"])
}

// Only the most recent task counts: an abandoned older input-required task
// must not be resumed once a newer task exists.
func TestSessionsClient_PendingTask_NothingPending(t *testing.T) {
	const tasksJSON = `{"data":[
		{"id":"task-stale","kind":"task","status":{"state":"input-required"}},
		{"id":"task-latest","kind":"task","status":{"state":"completed"}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tasksJSON))
	}))
	defer srv.Close()

	c := &SessionsClient{BaseURL: srv.URL}
	taskID, statusMessage, err := c.PendingTask(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Empty(t, taskID)
	require.Nil(t, statusMessage)
}

func TestSessionsClient_PendingTask_SessionGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &SessionsClient{BaseURL: srv.URL}
	taskID, _, err := c.PendingTask(t.Context(), "sess-1")
	require.NoError(t, err)
	require.Empty(t, taskID)
}
