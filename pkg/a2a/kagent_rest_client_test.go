package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKagentClient_Exists(t *testing.T) {
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

	c := &KagentClient{
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

func TestKagentClient_Exists_UnexpectedStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := &KagentClient{BaseURL: srv.URL}
	_, err := c.Exists(t.Context(), "x")
	require.Error(t, err)
}

type staticToken string

func (s staticToken) Token(_ context.Context) (string, error) { return string(s), nil }

func TestKagentClient_Delete(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	c := &KagentClient{
		BaseURL:     srv.URL,
		TokenSource: staticToken("tok-123"),
	}

	require.NoError(t, c.Delete(t.Context(), "sess-1"))
	require.Equal(t, http.MethodDelete, gotMethod)
	require.Equal(t, "/api/sessions/sess-1", gotPath)
	require.Equal(t, "Bearer tok-123", gotAuth, "the caller's token must be forwarded")

	// A missing session is success: it is gone either way.
	status = http.StatusNotFound
	require.NoError(t, c.Delete(t.Context(), "sess-1"))

	status = http.StatusInternalServerError
	require.Error(t, c.Delete(t.Context(), "sess-1"))
}

func TestKagentClient_AgentModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/agents/kagent/sre-agent", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"model":"gpt-5","modelProvider":"OpenAI"}}`))
	}))
	defer srv.Close()

	c := &KagentClient{BaseURL: srv.URL, TokenSource: staticToken("tok")}
	model, provider, err := c.AgentModel(t.Context(), "kagent/sre-agent")
	require.NoError(t, err)
	require.Equal(t, "gpt-5", model)
	require.Equal(t, "OpenAI", provider)
}

// A BYO agent yields empty model fields with a 200; that is not an error.
func TestKagentClient_AgentModel_EmptyForBYO(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"type":"BYO"}}`))
	}))
	defer srv.Close()

	c := &KagentClient{BaseURL: srv.URL}
	model, provider, err := c.AgentModel(t.Context(), "kagent/byo-agent")
	require.NoError(t, err)
	require.Empty(t, model)
	require.Empty(t, provider)
}

func TestKagentClient_AgentModel_BadRefAndStatus(t *testing.T) {
	c := &KagentClient{BaseURL: "http://unused"}
	_, _, err := c.AgentModel(t.Context(), "no-namespace")
	require.ErrorContains(t, err, "namespace/name")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c = &KagentClient{BaseURL: srv.URL}
	_, _, err = c.AgentModel(t.Context(), "kagent/missing")
	require.ErrorContains(t, err, "unexpected status 404")
}

func TestKagentClient_ListAgents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/agents", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":false,"data":[
			{"id":"1","agent":{"metadata":{"name":"sre-agent","namespace":"kagent","annotations":{"ui.giantswarm.io/display-name":"SRE Agent"}},"spec":{"description":"Investigates infra issues"}}},
			{"id":"2","agent":{"metadata":{"name":"k8s-agent","namespace":"kagent"},"spec":{}}},
			{"id":"3","agent":{"metadata":{}}}
		]}`))
	}))
	defer srv.Close()

	c := &KagentClient{BaseURL: srv.URL, TokenSource: staticToken("tok")}
	agents, err := c.ListAgents(t.Context())
	require.NoError(t, err)
	// The nameless third entry is skipped: it cannot be selected or displayed.
	require.Equal(t, []AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", DisplayName: "SRE Agent", Description: "Investigates infra issues"},
		{Name: "k8s-agent", Namespace: "kagent"},
	}, agents)
}

func TestKagentClient_ListAgents_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := &KagentClient{BaseURL: srv.URL}
	_, err := c.ListAgents(t.Context())
	require.ErrorContains(t, err, "unexpected status 502")
}
