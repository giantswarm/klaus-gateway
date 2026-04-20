package operator_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/operator"
)

type mcpRequest struct {
	Method string `json:"method"`
	Params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

func TestOperator_Create(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer s3cret", r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var req mcpRequest
		require.NoError(t, json.Unmarshal(body, &req))
		require.Equal(t, "tools/call", req.Method)
		require.Equal(t, "create_instance", req.Params.Name)
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]any{"name": "inst-x", "base_url": "http://inst-x"},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	m, err := operator.New(srv.URL, "s3cret")
	require.NoError(t, err)
	ref, err := m.Create(context.Background(), lifecycle.CreateSpec{Name: "inst-x"})
	require.NoError(t, err)
	require.Equal(t, "inst-x", ref.Name)
	require.Equal(t, "http://inst-x", ref.BaseURL)
}

func TestOperator_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]any{"code": 404, "message": "no such instance"},
		})
	}))
	t.Cleanup(srv.Close)

	m, err := operator.New(srv.URL, "")
	require.NoError(t, err)
	_, err = m.Get(context.Background(), "missing")
	require.ErrorIs(t, err, lifecycle.ErrNotFound)
}
