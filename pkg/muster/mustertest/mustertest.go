// Package mustertest is a fake muster for tests: an MCP server whose call_tool
// answers each tool from a function, wrapped in call_tool's envelope the way
// muster returns a dispatched tool's result. A tool that needs the person's
// sign-in answers with a URL elicitation, as muster does for a client that
// declared URL mode elicitation.
package mustertest

import (
	"context"
	"encoding/json"
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

// SignIn is the URL elicitation a call is answered with.
type SignIn struct {
	Message string
	URL     string
}

// Call is one tools/call the fake served. Accepted is a retry carrying the
// person's accept of the sign-in the call was answered with.
type Call struct {
	Bearer   string
	Tool     string
	Args     map[string]any
	Accepted bool
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

	mu    sync.Mutex
	calls []Call
}

type bearerKey struct{}

// New starts a fake muster; Close stops it.
func New(tools map[string]func(args map[string]any) Answer) *Server {
	s := &Server{Tools: tools}
	mcpServer := server.NewMCPServer("mustertest", "test")
	mcpServer.AddTool(mcp.NewTool("call_tool"), s.callTool)
	handler := server.NewStreamableHTTPServer(mcpServer,
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, bearerKey{}, r.Header.Get("Authorization"))
		}),
	)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
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
	call := Call{Bearer: bearer, Tool: tool, Args: inner}
	if answer := server.ElicitationResponse(req.Params.InputResponses, signInKey); answer != nil && req.Params.RequestState == signInState {
		if answer.Action != mcp.ElicitationResponseActionAccept {
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
		return server.NewInputRequestBuilder(signInState).Elicit(signInKey, mcp.ElicitationParams{
			Mode:          mcp.ElicitationModeURL,
			Message:       a.SignIn.Message,
			URL:           a.SignIn.URL,
			ElicitationID: "mustertest-" + call.Tool,
		}).ToolResult(), nil
	}
	return result(a), nil
}

func declaredURLElicitation(ctx context.Context) bool {
	info := server.RequestProtocolInfoFromContext(ctx)
	if info == nil || info.ClientCapabilities == nil {
		return false
	}
	e := info.ClientCapabilities.Elicitation
	return e != nil && e.URL != nil
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
