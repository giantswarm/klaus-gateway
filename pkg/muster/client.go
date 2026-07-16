// Package muster is a minimal MCP client for muster's streamable-HTTP /mcp
// endpoint. Its scope is deliberately tiny: the gateway only ever reads the
// auth://status resource and calls the core_auth_login / core_auth_logout
// tools with a linked user's own bearer token — it never proxies agent tool
// calls (those flow through kagent). Sessions are per bearer: muster's
// mcp-oauth front door keys tool/resource visibility on the validated token,
// so each user's calls run over their own MCP session.
package muster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giantswarm/klaus-gateway/internal/version"
)

// protocolVersion is the MCP protocol revision offered on initialize and
// echoed on subsequent requests.
const protocolVersion = "2025-03-26"

// sessionTTL bounds how long a cached Mcp-Session-Id is reused before a fresh
// initialize. Soft: a stale session inside the window is re-initialized on
// the 404 the server answers with.
const sessionTTL = 30 * time.Minute

// JSON-RPC envelope constants.
const (
	jsonrpcVersion  = "2.0"
	contentTypeText = "text"
	toolNameKey     = "name"
)

// errStaleSession marks an RPC rejected because the server no longer knows
// the Mcp-Session-Id; the caller re-initializes once and retries.
var errStaleSession = errors.New("muster: stale mcp session")

// Client speaks JSON-RPC MCP over streamable HTTP to muster.
type Client struct {
	// BaseURL is the muster MCP endpoint, e.g. https://muster.example.com/mcp.
	BaseURL string
	// HTTP overrides the transport. Nil uses a 15s-timeout default.
	HTTP   *http.Client
	Logger *slog.Logger

	rpcID atomic.Int64

	mu       sync.Mutex
	sessions map[string]sessionEntry // bearer token -> established MCP session
}

// sessionEntry is a cached Mcp-Session-Id with its soft expiry.
type sessionEntry struct {
	id      string
	expires time.Time
}

// ToolResult is the text content of an MCP tools/call result.
type ToolResult struct {
	IsError bool
	Text    string
}

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// ReadResource reads an MCP resource and returns the first content item's
// MIME type and text.
func (c *Client) ReadResource(ctx context.Context, token, uri string) (mime, text string, err error) {
	raw, err := c.call(ctx, token, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", "", err
	}
	var result struct {
		Contents []struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", fmt.Errorf("muster: decode resources/read result: %w", err)
	}
	if len(result.Contents) == 0 {
		return "", "", fmt.Errorf("muster: resource %s returned no contents", uri)
	}
	return result.Contents[0].MIMEType, result.Contents[0].Text, nil
}

// CallTool invokes an MCP tool and returns its concatenated text content.
func (c *Client) CallTool(ctx context.Context, token, name string, args map[string]any) (*ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.call(ctx, token, "tools/call", map[string]any{toolNameKey: name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("muster: decode tools/call result: %w", err)
	}
	var texts []string
	for _, item := range result.Content {
		if item.Type == contentTypeText && item.Text != "" {
			texts = append(texts, item.Text)
		}
	}
	return &ToolResult{IsError: result.IsError, Text: strings.Join(texts, "\n")}, nil
}

// call runs one JSON-RPC method over the bearer's MCP session, establishing
// or re-establishing the session as needed. One retry on a stale session.
func (c *Client) call(ctx context.Context, token, method string, params any) (json.RawMessage, error) {
	sessionID, err := c.session(ctx, token)
	if err != nil {
		return nil, err
	}
	raw, err := c.rpc(ctx, token, sessionID, method, params)
	if errors.Is(err, errStaleSession) {
		c.dropSession(token)
		if sessionID, err = c.session(ctx, token); err != nil {
			return nil, err
		}
		raw, err = c.rpc(ctx, token, sessionID, method, params)
	}
	return raw, err
}

// session returns the cached MCP session for the bearer, initializing one on
// miss or expiry.
func (c *Client) session(ctx context.Context, token string) (string, error) {
	c.mu.Lock()
	entry, ok := c.sessions[token]
	c.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.id, nil
	}
	return c.initialize(ctx, token)
}

func (c *Client) dropSession(token string) {
	c.mu.Lock()
	delete(c.sessions, token)
	c.mu.Unlock()
}

// initialize performs the MCP handshake for the bearer and caches the
// Mcp-Session-Id the server assigns. The initialized notification is sent
// best-effort: the server accepts requests without it, and a failure there
// must not fail the caller's actual request.
func (c *Client) initialize(ctx context.Context, token string) (string, error) {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "klaus-gateway",
			"version": version.Version(),
		},
	}
	resp, err := c.post(ctx, token, "", jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      c.rpcID.Add(1),
		Method:  "initialize",
		Params:  params,
	})
	if err != nil {
		return "", fmt.Errorf("muster: initialize: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	sessionID := resp.Header.Get("Mcp-Session-Id")
	if _, err := c.decodeResponse(resp); err != nil {
		return "", fmt.Errorf("muster: initialize: %w", err)
	}
	if sessionID == "" {
		return "", errors.New("muster: initialize response carried no Mcp-Session-Id")
	}

	if err := c.notifyInitialized(ctx, token, sessionID); err != nil {
		c.logger().Debug("muster: initialized notification failed", "error", err)
	}

	c.mu.Lock()
	if c.sessions == nil {
		c.sessions = make(map[string]sessionEntry)
	}
	// Opportunistic pruning keeps the map bounded to active bearers.
	now := time.Now()
	for k, e := range c.sessions {
		if now.After(e.expires) {
			delete(c.sessions, k)
		}
	}
	c.sessions[token] = sessionEntry{id: sessionID, expires: now.Add(sessionTTL)}
	c.mu.Unlock()
	return sessionID, nil
}

func (c *Client) notifyInitialized(ctx context.Context, token, sessionID string) error {
	resp, err := c.post(ctx, token, sessionID, jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		Method:  "notifications/initialized",
	})
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// rpc sends one request over an established session and decodes the matching
// response.
func (c *Client) rpc(ctx context.Context, token, sessionID, method string, params any) (json.RawMessage, error) {
	id := c.rpcID.Add(1)
	resp, err := c.post(ctx, token, sessionID, jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("muster: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := c.decodeResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("muster: %s: %w", method, err)
	}
	return raw, nil
}

// post sends the JSON-RPC envelope with the bearer, session, and protocol
// headers, mapping session-loss statuses to errStaleSession.
func (c *Client) post(ctx context.Context, token, sessionID string, req jsonrpcRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("MCP-Protocol-Version", protocolVersion)
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	// The streamable-HTTP transport answers 404 (and some servers 400) when
	// the session ID is no longer known.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		if sessionID != "" {
			return nil, fmt.Errorf("%w: status %d: %s", errStaleSession, resp.StatusCode, string(snippet))
		}
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(snippet))
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusAccepted {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(snippet))
	}
	return resp, nil
}

// decodeResponse extracts the JSON-RPC result from a plain JSON body or an
// SSE stream (the streamable-HTTP transport may answer either), returning the
// raw result or the mapped JSON-RPC error.
func (c *Client) decodeResponse(resp *http.Response) (json.RawMessage, error) {
	contentType := resp.Header.Get("Content-Type")

	var rpcResp jsonrpcResponse
	if strings.HasPrefix(contentType, "text/event-stream") {
		found := false
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			var candidate jsonrpcResponse
			if err := json.Unmarshal([]byte(data), &candidate); err != nil {
				continue
			}
			// Server-initiated notifications may interleave; the response to
			// our request is the event carrying a result or error.
			if candidate.Result != nil || candidate.Error != nil {
				rpcResp = candidate
				found = true
				break
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read sse response: %w", err)
		}
		if !found {
			return nil, errors.New("sse stream ended without a response")
		}
	} else {
		if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}
