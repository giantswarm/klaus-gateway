// Package operator implements the lifecycle.Manager backed by the klaus-operator
// MCP endpoint. It talks JSON-RPC over HTTP, calling the MCP tools
// `create_instance`, `get_instance`, `list_instances`, `stop_instance`.
package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

// Manager speaks JSON-RPC MCP to klaus-operator.
type Manager struct {
	httpClient *http.Client
	endpoint   string
	token      string

	rpcID atomic.Int64
}

// Option customises a Manager.
type Option func(*Manager)

// WithHTTPClient lets tests substitute an httptest-backed client.
func WithHTTPClient(c *http.Client) Option { return func(m *Manager) { m.httpClient = c } }

// New returns a Manager for the given MCP endpoint. Token may be empty.
func New(endpoint, token string, opts ...Option) (*Manager, error) {
	if endpoint == "" {
		return nil, errors.New("operator: endpoint is required")
	}
	m := &Manager{
		httpClient: &http.Client{},
		endpoint:   endpoint,
		token:      token,
	}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// jsonrpcRequest is a trimmed MCP call envelope.
type jsonrpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int64          `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
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

func (m *Manager) call(ctx context.Context, tool string, args map[string]any, out any) error {
	id := m.rpcID.Add(1)
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      tool,
			"arguments": args,
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if m.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+m.token)
	}
	resp, err := m.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("operator %s: status %d: %s", tool, resp.StatusCode, string(snippet))
	}
	var rpcResp jsonrpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return fmt.Errorf("decode %s: %w", tool, err)
	}
	if rpcResp.Error != nil {
		if rpcResp.Error.Code == 404 {
			return lifecycle.ErrNotFound
		}
		return fmt.Errorf("operator %s: %s", tool, rpcResp.Error.Message)
	}
	if out != nil && len(rpcResp.Result) > 0 {
		return json.Unmarshal(rpcResp.Result, out)
	}
	return nil
}

type operatorInstance struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url,omitempty"`
	MCPURL  string `json:"mcp_url,omitempty"`
	Status  string `json:"status,omitempty"`
}

func (o operatorInstance) toRef() lifecycle.InstanceRef {
	return lifecycle.InstanceRef{Name: o.Name, BaseURL: o.BaseURL, MCPURL: o.MCPURL, Status: o.Status}
}

// Get calls get_instance.
func (m *Manager) Get(ctx context.Context, name string) (lifecycle.InstanceRef, error) {
	var inst operatorInstance
	if err := m.call(ctx, "get_instance", map[string]any{"name": name}, &inst); err != nil {
		return lifecycle.InstanceRef{}, err
	}
	if inst.Name == "" {
		return lifecycle.InstanceRef{}, lifecycle.ErrNotFound
	}
	return inst.toRef(), nil
}

// Create calls create_instance.
func (m *Manager) Create(ctx context.Context, spec lifecycle.CreateSpec) (lifecycle.InstanceRef, error) {
	args := map[string]any{
		"name":       spec.Name,
		"channel":    spec.Channel,
		"channel_id": spec.ChannelID,
		"user_id":    spec.UserID,
		"thread_id":  spec.ThreadID,
	}
	if len(spec.Metadata) > 0 {
		args["metadata"] = spec.Metadata
	}
	var inst operatorInstance
	if err := m.call(ctx, "create_instance", args, &inst); err != nil {
		return lifecycle.InstanceRef{}, err
	}
	return inst.toRef(), nil
}

// List calls list_instances.
func (m *Manager) List(ctx context.Context) ([]lifecycle.InstanceRef, error) {
	var raw []operatorInstance
	if err := m.call(ctx, "list_instances", map[string]any{}, &raw); err != nil {
		return nil, err
	}
	refs := make([]lifecycle.InstanceRef, 0, len(raw))
	for _, r := range raw {
		refs = append(refs, r.toRef())
	}
	return refs, nil
}

// Stop calls stop_instance.
func (m *Manager) Stop(ctx context.Context, name string) error {
	return m.call(ctx, "stop_instance", map[string]any{"name": name}, nil)
}
