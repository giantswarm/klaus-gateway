// Package mustertest is a fake muster for tests: a streamable-HTTP MCP
// endpoint whose call_tool answers each tool from a function, wrapped in
// call_tool's envelope the way muster returns a dispatched tool's result.
package mustertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Answer is a tool's result: its text, its structured content and whether it
// is an error. Undispatched makes it call_tool's own refusal, no envelope, as
// muster answers a tool it does not know.
type Answer struct {
	Text         string
	Structured   any
	IsError      bool
	Undispatched bool
}

// Call is one tools/call the fake served.
type Call struct {
	Bearer string
	Tool   string
	Args   map[string]any
}

// Server is the fake. Tools answers each tool by name; a tool it does not
// list is refused like muster refuses an unknown one.
type Server struct {
	*httptest.Server
	Tools map[string]func(args map[string]any) Answer

	mu    sync.Mutex
	calls []Call
}

// New starts a fake muster; Close stops it.
func New(tools map[string]func(args map[string]any) Answer) *Server {
	s := &Server{Tools: tools}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Calls are the tools/call requests served, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		return
	}
	var req struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "fake-session")
		reply(w, req.ID, map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}})
	case "tools/call":
		tool := req.Params.Arguments.Name
		s.mu.Lock()
		s.calls = append(s.calls, Call{Bearer: r.Header.Get("Authorization"), Tool: tool, Args: req.Params.Arguments.Arguments})
		answer, ok := s.Tools[tool]
		s.mu.Unlock()
		a := Answer{Text: "Tool execution failed: tool not found: " + tool, IsError: true, Undispatched: true}
		if ok {
			a = answer(req.Params.Arguments.Arguments)
		}
		reply(w, req.ID, result(a))
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func result(a Answer) map[string]any {
	inner := map[string]any{"isError": a.IsError, "content": []any{map[string]any{"type": "text", "text": a.Text}}}
	if a.Undispatched {
		return inner
	}
	if a.Structured != nil {
		inner["structuredContent"] = a.Structured
	}
	envelope, _ := json.Marshal(inner)
	return map[string]any{"isError": a.IsError, "content": []any{map[string]any{"type": "text", "text": string(envelope)}}}
}

func reply(w http.ResponseWriter, id, res any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
}
