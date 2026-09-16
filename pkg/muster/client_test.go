package muster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeMuster is a stateful streamable-HTTP MCP server: initialize hands out a
// session id, later requests must carry it and the bearer, tools/call answers
// from callResult, in JSON or SSE.
type fakeMuster struct {
	mu         sync.Mutex
	sse        bool
	callResult map[string]any
	bearers    []string
	calls      []map[string]any
	deleted    bool
	unauth     bool
}

func (f *fakeMuster) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		if f.unauth {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			f.deleted = r.Header.Get(sessionHeader) == "sess-1"
			w.WriteHeader(http.StatusOK)
			return
		}
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Method == "initialize" {
			w.Header().Set(sessionHeader, "sess-1")
			f.reply(w, req.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "muster"}})
			return
		}
		if r.Header.Get(sessionHeader) != "sess-1" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "tools/call" {
			f.calls = append(f.calls, req.Params)
			f.reply(w, req.ID, f.callResult)
			return
		}
		http.Error(w, "unexpected method "+req.Method, http.StatusBadRequest)
	})
}

func (f *fakeMuster) reply(w http.ResponseWriter, id, result any) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		// A notification without an id precedes the response, as a server may
		// interleave one; the client must skip it.
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func approved() map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": "review submitted"}}}
}

func TestCallTool_RunsAsThePersonAndClosesTheSession(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			fake := &fakeMuster{sse: sse, callResult: approved()}
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			res, err := NewClient(srv.URL+"/").CallTool(context.Background(), "id-token-alice", "x_repo_approve_change", map[string]any{"pr": 7})
			require.NoError(t, err)
			require.Equal(t, Result{Text: "review submitted"}, res)

			fake.mu.Lock()
			defer fake.mu.Unlock()
			require.Len(t, fake.calls, 1)
			require.Equal(t, "x_repo_approve_change", fake.calls[0]["name"])
			require.Equal(t, map[string]any{"pr": float64(7)}, fake.calls[0]["arguments"])
			for _, b := range fake.bearers {
				require.Equal(t, "Bearer id-token-alice", b, "every request carries the person's token")
			}
			require.True(t, fake.deleted, "the session muster issued is closed")
		})
	}
}

func TestCallTool_ToolRefusalIsAResultNotAnError(t *testing.T) {
	fake := &fakeMuster{callResult: map[string]any{
		"isError": true,
		"content": []any{map[string]any{"type": "text", "text": "alice is not a member of team-bumblebee"}},
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	res, err := NewClient(srv.URL).CallTool(context.Background(), "id-token-alice", "x_repo_approve_change", nil)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "alice is not a member of team-bumblebee", res.Text)
}

func TestCallTool_RefusedTokenIsAnHTTPError(t *testing.T) {
	fake := &fakeMuster{unauth: true}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := NewClient(srv.URL).CallTool(context.Background(), "stale", "x_repo_approve_change", nil)
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusUnauthorized, httpErr.Status)
}

func TestCallTool_NoBearerNoCall(t *testing.T) {
	fake := &fakeMuster{callResult: approved()}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := NewClient(srv.URL).CallTool(context.Background(), "", "x", nil)
	require.Error(t, err)
	require.Empty(t, fake.bearers)
}
