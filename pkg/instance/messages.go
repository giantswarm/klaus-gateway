package instance

import (
	"context"
	"encoding/json"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

// Message is a single stored turn as returned by the klaus MCP `messages` tool.
type Message struct {
	Role    string          `json:"role"`
	Content string          `json:"content"`
	Extra   json.RawMessage `json:"extra,omitempty"`
}

// MessagesResponse is what the MCP `messages` tool returns.
type MessagesResponse struct {
	Messages []Message `json:"messages"`
}

// Messages fetches the backlog for an instance. This is used by the web
// channel adapter on page load in the follow-up PR; the call is implemented
// and tested here so the adapter can depend on it without adding code.
func (c *Client) Messages(ctx context.Context, ref lifecycle.InstanceRef, threadID string) (MessagesResponse, error) {
	args := map[string]any{}
	if threadID != "" {
		args["thread_id"] = threadID
	}
	raw, err := c.CallMCPTool(ctx, ref, "messages", args)
	if err != nil {
		return MessagesResponse{}, err
	}
	var resp MessagesResponse
	if len(raw) == 0 {
		return resp, nil
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return MessagesResponse{}, err
	}
	return resp, nil
}
