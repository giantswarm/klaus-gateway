package muster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeMuster is an httptest-backed muster /mcp endpoint: it enforces the
// initialize handshake and Mcp-Session-Id echo, serves auth://status and the
// core auth tools, and can answer as SSE instead of plain JSON.
type fakeMuster struct {
	t *testing.T

	sse bool // answer requests as text/event-stream

	mu          sync.Mutex
	sessions    map[string]bool
	nextSession int
	initCalls   int
	readCalls   int
	statusJSON  string
	loginText   string
	loginIsErr  bool
	logoutText  string
	logoutIsErr bool
	wantBearer  string
}

func newFakeMuster(t *testing.T) (*fakeMuster, *Client) {
	t.Helper()
	f := &fakeMuster{
		t:          t,
		sessions:   map[string]bool{},
		statusJSON: `{"servers":[]}`,
		wantBearer: "user-token",
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, &Client{BaseURL: server.URL}
}

func (f *fakeMuster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	require.Equal(f.t, "Bearer "+f.wantBearer, r.Header.Get("Authorization"))

	var req struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params struct {
			URI       string         `json:"uri"`
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	require.NoError(f.t, json.NewDecoder(r.Body).Decode(&req))

	switch req.Method {
	case "initialize":
		f.initCalls++
		f.nextSession++
		id := fmt.Sprintf("sess-%d", f.nextSession)
		f.sessions[id] = true
		w.Header().Set("Mcp-Session-Id", id)
		f.respond(w, req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fake-muster", "version": "0.0.0"},
		})
		return
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if sid := r.Header.Get("Mcp-Session-Id"); !f.sessions[sid] {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	switch req.Method {
	case "resources/read":
		f.readCalls++
		require.Equal(f.t, AuthStatusURI, req.Params.URI)
		f.respond(w, req.ID, map[string]any{
			"contents": []map[string]any{{
				"uri":      AuthStatusURI,
				"mimeType": "application/json",
				"text":     f.statusJSON,
			}},
		})
	case "tools/call":
		var text string
		var isErr bool
		switch req.Params.Name {
		case loginTool:
			text, isErr = f.loginText, f.loginIsErr
		case logoutTool:
			text, isErr = f.logoutText, f.logoutIsErr
		default:
			f.t.Errorf("unexpected tool %q", req.Params.Name)
		}
		require.NotEmpty(f.t, req.Params.Arguments["server"])
		f.respond(w, req.ID, map[string]any{
			"isError": isErr,
			"content": []map[string]any{{"type": "text", "text": text}},
		})
	default:
		f.t.Errorf("unexpected method %q", req.Method)
	}
}

// dropSessions makes the server forget every established session, so the next
// request on a cached session answers 404.
func (f *fakeMuster) dropSessions() {
	f.mu.Lock()
	f.sessions = map[string]bool{}
	f.mu.Unlock()
}

func (f *fakeMuster) respond(w http.ResponseWriter, id int64, result any) {
	resultJSON, err := json.Marshal(result)
	require.NoError(f.t, err)
	body, err := json.Marshal(jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: resultJSON})
	require.NoError(f.t, err)

	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		// A notification event before the response exercises the client's
		// event filtering.
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

const statusFixture = `{"servers":[
	{"name":"gazelle-mcp-pro","status":"auth_required","issuer":"https://pro.example.com","scope":"repo","auth_tool":"core_auth_login"},
	{"name":"mcp-kubernetes","status":"auth_required","token_forwarding_enabled":true},
	{"name":"mcp-prometheus","status":"connected"}
]}`

func TestAuthStatus(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.statusJSON = statusFixture

	servers, err := client.AuthStatus(t.Context(), "user-token")
	require.NoError(t, err)
	require.Len(t, servers, 3)

	pro := servers[0]
	require.Equal(t, "gazelle-mcp-pro", pro.Name)
	require.True(t, pro.Connector())
	require.True(t, pro.NeedsAuth())

	// Forward-token backends are never connectors even when auth_required.
	require.False(t, servers[1].Connector())
	require.True(t, servers[1].NeedsAuth())

	require.False(t, servers[2].NeedsAuth())
}

func TestAuthStatus_SSEResponse(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.sse = true
	fake.statusJSON = statusFixture

	servers, err := client.AuthStatus(t.Context(), "user-token")
	require.NoError(t, err)
	require.Len(t, servers, 3)
}

func TestSessionReuse(t *testing.T) {
	fake, client := newFakeMuster(t)

	_, err := client.AuthStatus(t.Context(), "user-token")
	require.NoError(t, err)
	_, err = client.AuthStatus(t.Context(), "user-token")
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 1, fake.initCalls, "second call must reuse the cached session")
}

func TestStaleSessionRetry(t *testing.T) {
	fake, client := newFakeMuster(t)

	_, err := client.AuthStatus(t.Context(), "user-token")
	require.NoError(t, err)

	// The server forgets the session (restart); the next call must
	// re-initialize once and succeed.
	fake.dropSessions()
	_, err = client.AuthStatus(t.Context(), "user-token")
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 2, fake.initCalls)
}

func TestLoginURL(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.loginText = "Authentication Required\n\nServer: pro\n\nPlease sign in to connect to this server:\n\nhttps://pro.example.com/oauth/authorize?state=abc&code_challenge=xyz\n\nAfter signing in, run this tool again to complete the connection."

	url, err := client.LoginURL(t.Context(), "user-token", "gazelle-mcp-pro")
	require.NoError(t, err)
	require.Equal(t, "https://pro.example.com/oauth/authorize?state=abc&code_challenge=xyz", url)
}

func TestLoginURL_NoURL(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.loginText = "Server gazelle-mcp-pro is already authenticated."

	_, err := client.LoginURL(t.Context(), "user-token", "gazelle-mcp-pro")
	var noURL *NoLoginURLError
	require.ErrorAs(t, err, &noURL)
	require.Contains(t, noURL.Text, "already authenticated")
}

func TestLoginURL_ErrorResultWithURL(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.loginText = "Rate limited; see https://muster.example.com/docs for details."
	fake.loginIsErr = true

	_, err := client.LoginURL(t.Context(), "user-token", "gazelle-mcp-pro")
	var noURL *NoLoginURLError
	require.ErrorAs(t, err, &noURL, "an isError result must never yield a login URL")
}

func TestLogout(t *testing.T) {
	fake, client := newFakeMuster(t)
	fake.logoutText = "Disconnected from gazelle-mcp-pro."
	require.NoError(t, client.Logout(t.Context(), "user-token", "gazelle-mcp-pro"))

	fake.logoutIsErr = true
	fake.logoutText = "Cannot log out of an SSO server."
	err := client.Logout(t.Context(), "user-token", "gazelle-mcp-pro")
	require.ErrorContains(t, err, "SSO")
}
