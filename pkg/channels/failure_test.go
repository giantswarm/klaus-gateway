package channels_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// The classes are read off the wording of the layer that reported the
// failure; each case is the text that layer produces.
func TestClassifyFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want channels.FailureClass
	}{
		{"none", nil, channels.FailureNone},
		{"tool set init reset", errors.New(toolSetFailure), channels.FailureTools},
		{"tool set listing", errors.New(`failed to extract tools from the tool set "mcp_tool_set": failed to list MCP tools: connection lost again after reconnection`), channels.FailureTools},
		{"tool set refused by policy", errors.New(`failed to extract tools from the tool set "mcp_tool_set": failed to list MCP tools: failed to init MCP session: calling "initialize": 403 authorization failed`), channels.FailureTools},
		{"MCP server asks for authorization", errors.New("calling \"tools/list\": authorization required"), channels.FailureTools},
		{"controller unreachable", fmt.Errorf("slack: send completion: %w", errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing: dial tcp 10.0.0.3:8083: connect: connection refused"`)), channels.FailurePlatform},
		{"runtime connection reset", errors.New("runtime stream failed: read: connection reset by peer"), channels.FailurePlatform},
		{"anthropic overloaded", errors.New(`anthropic API error: POST "https://api.anthropic.com/v1/messages": 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), channels.FailureModel},
		{"model call reset", errors.New(`OpenAI chat completion request failed: Post "http://agentgateway/v1/chat/completions": read: connection reset by peer`), channels.FailureModel},
		{"gemini quota", errors.New("failed to call model: Error 429, Message: quota, Status: RESOURCE_EXHAUSTED"), channels.FailureModel},
		{"corrupt history", errors.New(`anthropic API error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"tool_use ids were found without tool_result blocks"}}`), channels.FailureModel},
		{"prompt guard", errors.New(`OpenAI chat completion request failed: 403 The request was rejected due to inappropriate content`), channels.FailurePolicy},
		{"gateway authorization", errors.New(`OpenAI chat completion request failed: 403 Forbidden: authorization failed`), channels.FailurePolicy},
		{"gateway rate limit", errors.New(`OpenAI chat completion request failed: 429 rate limit exceeded`), channels.FailurePolicy},
		{"unknown", errors.New("a2a: task ended with state failed"), channels.FailureUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, channels.ClassifyFailure(tc.err))
		})
	}
}

// Only a broken connection is worth a second attempt.
func TestFailureClass_Retryable(t *testing.T) {
	for class, want := range map[channels.FailureClass]bool{
		channels.FailureNone:     false,
		channels.FailureTools:    true,
		channels.FailurePlatform: true,
		channels.FailureModel:    false,
		channels.FailurePolicy:   false,
		channels.FailureUnknown:  false,
	} {
		require.Equal(t, want, class.Retryable(), class)
	}
}
