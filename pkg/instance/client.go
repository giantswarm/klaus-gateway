// Package instance provides an HTTP client for talking to a klaus instance
// over its OpenAI-compatible `/v1/*` surface and its MCP endpoint.
package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

// Client issues HTTP calls to klaus instances. One client is safe to share.
type Client struct {
	HTTP *http.Client

	// Upstream, when non-nil, rewrites requests to go through an upstream
	// (e.g. agentgateway) instead of directly to the instance. See
	// pkg/upstream.Agentgateway.Apply for the hook shape.
	Upstream UpstreamRewriter
}

// UpstreamRewriter adapts the request URL + headers for a fronting proxy.
type UpstreamRewriter interface {
	Apply(req *http.Request, ref lifecycle.InstanceRef)
}

// NewClient returns a Client with a default http.Client.
func NewClient() *Client { return &Client{HTTP: &http.Client{}} }

// StreamCompletion POSTs body to /v1/chat/completions and returns the raw
// response body (typically SSE). The caller is responsible for closing it.
func (c *Client) StreamCompletion(ctx context.Context, ref lifecycle.InstanceRef, body []byte) (io.ReadCloser, error) {
	if ref.BaseURL == "" && c.Upstream == nil {
		return nil, errors.New("instance: BaseURL is empty and no upstream is configured")
	}
	req, err := c.newRequest(ctx, http.MethodPost, ref, "/v1/chat/completions", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("instance %s: status %d: %s", ref.Name, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return resp.Body, nil
}

// CallMCPTool POSTs a tools/call request to the instance MCP endpoint and
// returns the decoded JSON result.
func (c *Client) CallMCPTool(ctx context.Context, ref lifecycle.InstanceRef, tool string, args map[string]any) (json.RawMessage, error) {
	endpoint := ref.MCPURL
	if endpoint == "" {
		if ref.BaseURL == "" {
			return nil, errors.New("instance: neither MCPURL nor BaseURL is set")
		}
		endpoint = strings.TrimRight(ref.BaseURL, "/") + "/mcp"
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Upstream != nil {
		c.Upstream.Apply(req, ref)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("mcp %s: status %d: %s", tool, resp.StatusCode, string(snippet))
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("mcp %s: %s", tool, envelope.Error.Message)
	}
	return envelope.Result, nil
}

// newRequest builds a request for path on ref.BaseURL (or the upstream).
func (c *Client) newRequest(ctx context.Context, method string, ref lifecycle.InstanceRef, path string, body []byte) (*http.Request, error) {
	if c.Upstream != nil {
		// Upstream rewrites the URL; start from a placeholder.
		req, err := http.NewRequestWithContext(ctx, method, "http://upstream.invalid"+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.Upstream.Apply(req, ref)
		return req, nil
	}
	base, err := url.Parse(ref.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	return http.NewRequestWithContext(ctx, method, base.String(), bytes.NewReader(body))
}
