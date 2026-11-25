// Package mustertest is a fake muster for tests: an MCP server whose call_tool
// answers each tool from a function, wrapped in call_tool's envelope the way
// muster returns a dispatched tool's result. A tool that needs the person's
// sign-in answers with a URL elicitation, as muster does for a client that
// declared URL mode elicitation.
package mustertest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Answer is a tool's result: its text, its structured content and whether it
// is an error. Undispatched makes it call_tool's own refusal, no envelope, as
// muster answers a tool it does not know. SignIn makes it muster's sign-in
// instead: the call does not run, and the person is asked to open the link.
type Answer struct {
	Text         string
	Structured   any
	IsError      bool
	Undispatched bool
	SignIn       *SignIn
}

// SignIn is the URL elicitation a call is answered with. ElicitationID is
// its elicitationId, "mustertest-<tool>" when empty.
type SignIn struct {
	Message       string
	URL           string
	ElicitationID string
}

// Call is one tools/call the fake served. Accepted is a retry carrying the
// person's accept of the sign-in the call was answered with, Declined one
// carrying their decline (the tool did not run). SessionID is the client's
// MCP session (Mcp-Session-Id), which muster binds a pending approval to.
type Call struct {
	Bearer    string
	SessionID string
	Tool      string
	Args      map[string]any
	Accepted  bool
	Declined  bool
}

// signInKey and signInState are the input request key and request state the
// fake asks a sign-in under; a retry must echo the state.
const (
	signInKey   = "sign_in"
	signInState = "mustertest-sign-in"
)

// Server is the fake. Tools answers each tool by name; a tool it does not
// list is refused like muster refuses an unknown one. CallTools answers a tool
// from the whole call, the caller's bearer and accept included, for a fake
// that acts as the caller; it is consulted before Tools.
type Server struct {
	*httptest.Server
	Tools     map[string]func(args map[string]any) Answer
	CallTools map[string]func(call Call) Answer
	// Legacy makes the fake speak protocol 2025-11-25 alone, as muster does
	// in its first tool approval release: every client connection is an MCP
	// session (Mcp-Session-Id, Call.SessionID), and a URL elicitation is the
	// JSON-RPC error -32042 instead of an input_required result.
	Legacy bool

	mu    sync.Mutex
	calls []Call
}

type (
	bearerKey  struct{}
	sessionKey struct{}
)

// New starts a fake muster; Close stops it.
func New(tools map[string]func(args map[string]any) Answer) *Server {
	s := &Server{Tools: tools}
	mcpServer := server.NewMCPServer("mustertest", "test")
	mcpServer.AddTool(mcp.NewTool("call_tool"), s.callTool)
	// muster is stateful: every client connection gets its own MCP session
	// (Mcp-Session-Id), which a pending approval is bound to.
	handler := server.NewStreamableHTTPServer(mcpServer,
		server.WithStateful(true),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			ctx = context.WithValue(ctx, sessionKey{}, r.Header.Get(server.HeaderKeySessionID))
			return context.WithValue(ctx, bearerKey{}, r.Header.Get("Authorization"))
		}),
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Legacy && legacyRefusesDiscover(w, r) {
			return
		}
		handler.ServeHTTP(w, r)
	}))
	s.Server = httptest.NewServer(mux)
	return s
}

// Calls are the tools/call requests served, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func (s *Server) callTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	tool, _ := args["name"].(string)
	inner, _ := args["arguments"].(map[string]any)
	bearer, _ := ctx.Value(bearerKey{}).(string)
	sessionID, _ := ctx.Value(sessionKey{}).(string)
	call := Call{Bearer: bearer, SessionID: sessionID, Tool: tool, Args: inner}
	if answer := server.ElicitationResponse(req.Params.InputResponses, signInKey); answer != nil && req.Params.RequestState == signInState {
		if answer.Action != mcp.ElicitationResponseActionAccept {
			call.Declined = true
			s.mu.Lock()
			s.calls = append(s.calls, call)
			s.mu.Unlock()
			return mcp.NewToolResultError("The tool did not run: the person answered " + string(answer.Action) + " to signing in."), nil
		}
		call.Accepted = true
	}

	s.mu.Lock()
	s.calls = append(s.calls, call)
	byCall, viaCall := s.CallTools[call.Tool]
	byArgs, ok := s.Tools[call.Tool]
	s.mu.Unlock()
	a := Answer{Text: "Tool execution failed: tool not found: " + call.Tool, IsError: true, Undispatched: true}
	switch {
	case viaCall:
		a = byCall(call)
	case ok:
		a = byArgs(call.Args)
	}
	if a.SignIn != nil {
		if !declaredURLElicitation(ctx) {
			return mcp.NewToolResultError("auth_required: the client did not declare URL elicitation"), nil
		}
		elicitationID := a.SignIn.ElicitationID
		if elicitationID == "" {
			elicitationID = "mustertest-" + call.Tool
		}
		if s.Legacy {
			return nil, mcp.URLElicitationRequiredError{Elicitations: []mcp.ElicitationParams{{
				Mode: mcp.ElicitationModeURL, Message: a.SignIn.Message, URL: a.SignIn.URL, ElicitationID: elicitationID,
			}}}
		}
		return server.NewInputRequestBuilder(signInState).Elicit(signInKey, mcp.ElicitationParams{
			Mode:          mcp.ElicitationModeURL,
			Message:       a.SignIn.Message,
			URL:           a.SignIn.URL,
			ElicitationID: elicitationID,
		}).ToolResult(), nil
	}
	return result(a), nil
}

func declaredURLElicitation(ctx context.Context) bool {
	var caps *mcp.ClientCapabilities
	if info := server.RequestProtocolInfoFromContext(ctx); info != nil && info.ClientCapabilities != nil {
		caps = info.ClientCapabilities
	} else if session, ok := server.ClientSessionFromContext(ctx).(server.SessionWithClientInfo); ok {
		// 2025-11-25: the capabilities the session's initialize declared.
		c := session.GetClientCapabilities()
		caps = &c
	}
	return caps != nil && caps.Elicitation != nil && caps.Elicitation.URL != nil
}

func result(a Answer) *mcp.CallToolResult {
	inner := map[string]any{"isError": a.IsError, "content": []any{map[string]any{"type": "text", "text": a.Text}}}
	if a.Undispatched {
		return &mcp.CallToolResult{IsError: a.IsError, Content: []mcp.Content{mcp.NewTextContent(a.Text)}}
	}
	if a.Structured != nil {
		inner["structuredContent"] = a.Structured
	}
	envelope, _ := json.Marshal(inner)
	return &mcp.CallToolResult{IsError: a.IsError, Content: []mcp.Content{mcp.NewTextContent(string(envelope))}}
}

// legacyRefusesDiscover answers a server/discover request as a server older
// than 2026-07-28 does, with method not found, so the client falls back to
// initialize on 2025-11-25; it reports whether it answered.
func legacyRefusesDiscover(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(body, &req) != nil || req.Method != string(mcp.MethodServerDiscover) {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"error":{"code":-32601,"message":"Method not found"}}`))
	return true
}
