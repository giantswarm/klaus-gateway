// Package muster calls one muster tool as a person. Each call is its own MCP
// streamable-HTTP exchange against muster's /mcp endpoint — initialize,
// tools/call, session close — authenticated with the person's dex id_token, so
// muster sees the person and not the gateway and the tool runs under the
// person's own grants.
package muster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	sessionHeader   = "Mcp-Session-Id"
	protocolVersion = "2025-03-26"
	maxResponse     = 1 << 20
	defaultTimeout  = 60 * time.Second
)

// Result is a tool's answer: its text content joined, and whether the tool
// reported it as an error (a refusal is an error result, not a transport
// failure).
type Result struct {
	Text    string
	IsError bool
}

// Client calls muster tools. One client is safe to share.
type Client struct {
	// BaseURL is muster's base URL; the MCP endpoint is BaseURL + /mcp.
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a Client for the muster at baseURL.
func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: defaultTimeout}}
}

// CallTool calls tool with args as the person whose bearer token is given.
// The transport is initialized per call and the session, when muster issues
// one, is closed afterwards.
func (c *Client) CallTool(ctx context.Context, bearer, tool string, args map[string]any) (Result, error) {
	if bearer == "" {
		return Result{}, errors.New("muster: no bearer token for the tool call")
	}
	if args == nil {
		args = map[string]any{}
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/mcp"
	s := session{client: c, endpoint: endpoint, bearer: bearer}

	if _, err := s.request(ctx, 1, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "klaus-gateway", "version": "team-review"},
	}); err != nil {
		return Result{}, fmt.Errorf("muster: initialize: %w", err)
	}
	defer s.close()
	if err := s.notify(ctx, "notifications/initialized"); err != nil {
		return Result{}, fmt.Errorf("muster: initialized: %w", err)
	}

	raw, err := s.request(ctx, 2, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return Result{}, fmt.Errorf("muster: call %s: %w", tool, err)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{}, fmt.Errorf("muster: call %s: decode result: %w", tool, err)
	}
	var texts []string
	for _, part := range result.Content {
		if part.Type == "text" && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return Result{Text: strings.Join(texts, "\n"), IsError: result.IsError}, nil
}

// session is one streamable-HTTP exchange: the session id muster hands out at
// initialize rides on every later request.
type session struct {
	client    *Client
	endpoint  string
	bearer    string
	sessionID string
}

// request sends one JSON-RPC request and returns its result. A JSON-RPC error
// is returned as an error carrying the server's message.
func (s *session) request(ctx context.Context, id int, method string, params any) (json.RawMessage, error) {
	resp, err := s.post(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := resp.Header.Get(sessionHeader); sid != "" {
		s.sessionID = sid
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := decodeRPCResponse(resp, &envelope); err != nil {
		return nil, err
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("%s (code %d)", envelope.Error.Message, envelope.Error.Code)
	}
	return envelope.Result, nil
}

// notify sends a JSON-RPC notification; the server answers with no body.
func (s *session) notify(ctx context.Context, method string) error {
	resp, err := s.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (s *session) post(ctx context.Context, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+s.bearer)
	if s.sessionID != "" {
		req.Header.Set(sessionHeader, s.sessionID)
	}
	resp, err := s.client.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	return resp, nil
}

// close ends the session muster issued, best-effort; a server that issued
// none (stateless) gets no request.
func (s *session) close() {
	if s.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.bearer)
	req.Header.Set(sessionHeader, s.sessionID)
	if resp, err := s.client.HTTP.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// HTTPError is a non-2xx answer from muster's MCP endpoint; 401/403 mean the
// person's token was refused.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("status %d", e.Status)
	}
	return fmt.Sprintf("status %d: %s", e.Status, e.Body)
}

// decodeRPCResponse reads the JSON-RPC response out of either a JSON body or
// an SSE stream (the first data: event carrying a response).
func decodeRPCResponse(resp *http.Response, into any) error {
	body := io.LimitReader(resp.Body, maxResponse)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return json.NewDecoder(body).Decode(into)
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponse)
	var data strings.Builder
	flush := func() (bool, error) {
		if data.Len() == 0 {
			return false, nil
		}
		raw := data.String()
		data.Reset()
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		// Notifications the server interleaves carry no id; the response does.
		if err := json.Unmarshal([]byte(raw), &probe); err != nil || len(probe.ID) == 0 || string(probe.ID) == "null" {
			return false, nil
		}
		return true, json.Unmarshal([]byte(raw), into)
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if done, err := flush(); done || err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if done, err := flush(); done || err != nil {
		return err
	}
	return errors.New("event stream ended without a response")
}
