package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentsClient_AgentModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/agents/kagent/sre-agent", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"model":"gpt-5","modelProvider":"OpenAI"}}`))
	}))
	defer srv.Close()

	c := &AgentsClient{BaseURL: srv.URL, TokenSource: tokenFunc(func() string { return "tok" })}
	model, provider, err := c.AgentModel(t.Context(), "kagent/sre-agent")
	require.NoError(t, err)
	require.Equal(t, "gpt-5", model)
	require.Equal(t, "OpenAI", provider)
}

// A BYO agent yields empty model fields with a 200; that is not an error.
func TestAgentsClient_AgentModel_EmptyForBYO(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"type":"BYO"}}`))
	}))
	defer srv.Close()

	c := &AgentsClient{BaseURL: srv.URL}
	model, provider, err := c.AgentModel(t.Context(), "kagent/byo-agent")
	require.NoError(t, err)
	require.Empty(t, model)
	require.Empty(t, provider)
}

func TestAgentsClient_AgentModel_BadRefAndStatus(t *testing.T) {
	c := &AgentsClient{BaseURL: "http://unused"}
	_, _, err := c.AgentModel(t.Context(), "no-namespace")
	require.ErrorContains(t, err, "namespace/name")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c = &AgentsClient{BaseURL: srv.URL}
	_, _, err = c.AgentModel(t.Context(), "kagent/missing")
	require.ErrorContains(t, err, "unexpected status 404")
}

func TestAgentsClient_ListAgents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/agents", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":false,"data":[
			{"id":"1","agent":{"metadata":{"name":"sre-agent","namespace":"kagent"},"spec":{"description":"Investigates infra issues"}}},
			{"id":"2","agent":{"metadata":{"name":"k8s-agent","namespace":"kagent"},"spec":{}}},
			{"id":"3","agent":{"metadata":{}}}
		]}`))
	}))
	defer srv.Close()

	c := &AgentsClient{BaseURL: srv.URL, TokenSource: tokenFunc(func() string { return "tok" })}
	agents, err := c.ListAgents(t.Context())
	require.NoError(t, err)
	// The nameless third entry is skipped: it cannot be selected or displayed.
	require.Equal(t, []AgentInfo{
		{Name: "sre-agent", Namespace: "kagent", Description: "Investigates infra issues"},
		{Name: "k8s-agent", Namespace: "kagent"},
	}, agents)
}

func TestAgentsClient_ListAgents_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := &AgentsClient{BaseURL: srv.URL}
	_, err := c.ListAgents(t.Context())
	require.ErrorContains(t, err, "unexpected status 502")
}

// tokenFunc adapts a func to TokenSource for tests.
type tokenFunc func() string

func (f tokenFunc) Token(context.Context) (string, error) { return f(), nil }
