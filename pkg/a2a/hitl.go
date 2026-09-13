package a2a

import (
	"encoding/json"
	"fmt"
	"slices"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
)

// HITLExtensionURI identifies kagent's human-in-the-loop A2A extension. A
// client requests it on every call (the A2A-Extensions service parameter, gRPC
// metadata a2a-extensions); the agent's runtime then pauses a tool that needs
// approval — or its built-in ask_user tool — as a task in the input-required
// state whose status message carries a typed request payload. Without the
// request the runtime answers with a plain text notice and nothing reaches the
// channel to decide on.
const HITLExtensionURI = "https://kagent.dev/extensions/hitl/v1"

// HITL payload type discriminators (the "type" field of the extension payload).
const (
	HITLTypeToolApprovalRequest  = "tool_approval_request"
	HITLTypeAskUserRequest       = "ask_user_request"
	HITLTypeToolApprovalResponse = "tool_approval_response"
	HITLTypeAskUserResponse      = "ask_user_response"
)

// AskUserToolName is the name of kagent's built-in question tool, the one
// HITL request that carries questions instead of a tool call to approve.
const AskUserToolName = "ask_user"

// HITLTool describes one tool invocation awaiting a human decision.
type HITLTool struct {
	ID     string         `json:"id"`
	CallID string         `json:"call_id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
}

// NestedHITLRequest identifies a request propagated from a child agent. The
// decision is made on its Tools, not on the parent's.
type NestedHITLRequest struct {
	SubagentName string     `json:"subagent_name,omitempty"`
	TaskID       string     `json:"task_id,omitempty"`
	ContextID    string     `json:"context_id,omitempty"`
	Tools        []HITLTool `json:"tools"`
}

// ToolApprovalRequest is the payload of an input-required task paused on one
// or more tool calls that need approval.
type ToolApprovalRequest struct {
	Type   string             `json:"type"`
	Hint   string             `json:"hint,omitempty"`
	Tools  []HITLTool         `json:"tools"`
	Nested *NestedHITLRequest `json:"nested,omitempty"`
}

// DecidedTools returns the tools a decision has to cover: the nested child's
// tools when the request was propagated from a sub-agent, the request's own
// otherwise.
func (r *ToolApprovalRequest) DecidedTools() []HITLTool {
	if r.Nested != nil {
		return r.Nested.Tools
	}
	return r.Tools
}

// ToolApproval is one human decision for a requested tool invocation.
type ToolApproval struct {
	ID              string `json:"id"`
	Approved        bool   `json:"approved"`
	RejectionReason string `json:"rejection_reason,omitempty"`
}

// ToolApprovalResponse answers a ToolApprovalRequest: exactly one decision per
// requested tool.
type ToolApprovalResponse struct {
	Type      string         `json:"type"`
	Approvals []ToolApproval `json:"approvals"`
}

// HITLQuestion is one question of an ask_user request.
type HITLQuestion struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
	Multiple bool     `json:"multiple"`
}

// AskUserRequest is the payload of an input-required task paused on the
// agent's ask_user tool.
type AskUserRequest struct {
	Type      string             `json:"type"`
	ID        string             `json:"id"`
	Questions []HITLQuestion     `json:"questions"`
	Nested    *NestedHITLRequest `json:"nested,omitempty"`
}

// ResponseID is the correlation id an AskUserResponse must carry: the nested
// child's ask_user tool id when the question was propagated from a sub-agent,
// the request's own id otherwise.
func (r *AskUserRequest) ResponseID() string {
	if r.Nested != nil && len(r.Nested.Tools) == 1 {
		return r.Nested.Tools[0].ID
	}
	return r.ID
}

// AskUserAnswer holds the selections for one question, positional with the
// request's questions.
type AskUserAnswer struct {
	Answer []string `json:"answer"`
}

// AskUserResponse answers an AskUserRequest.
type AskUserResponse struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Answers []AskUserAnswer `json:"answers,omitempty"`
}

// HITLRequest is the parsed request of a paused task: exactly one of the two
// fields is set.
type HITLRequest struct {
	ToolApproval *ToolApprovalRequest
	AskUser      *AskUserRequest
}

// ParseHITLRequest reads the HITL request the agent attached to an
// input-required status message. It returns nil when the message carries no
// HITL extension payload (a plain-text prompt, or a runtime that did not
// activate the extension).
func ParseHITLRequest(msg *a2apkg.Message) (*HITLRequest, error) {
	raw, ok := hitlPayload(msg)
	if !ok {
		return nil, nil
	}
	switch raw["type"] {
	case HITLTypeToolApprovalRequest:
		var req ToolApprovalRequest
		if err := decodeHITLPayload(raw, &req); err != nil {
			return nil, fmt.Errorf("decode tool approval request: %w", err)
		}
		if len(req.DecidedTools()) == 0 {
			return nil, fmt.Errorf("tool approval request has no tools")
		}
		return &HITLRequest{ToolApproval: &req}, nil
	case HITLTypeAskUserRequest:
		var req AskUserRequest
		if err := decodeHITLPayload(raw, &req); err != nil {
			return nil, fmt.Errorf("decode ask_user request: %w", err)
		}
		if req.ResponseID() == "" {
			return nil, fmt.Errorf("ask_user request has no id")
		}
		return &HITLRequest{AskUser: &req}, nil
	default:
		return nil, nil
	}
}

// AttachHITL puts a typed HITL payload on msg: the extension is declared on
// the message and the payload lands under the extension URI in its metadata,
// the shape kagent's runtime resolves a paused task from.
func AttachHITL(msg *a2apkg.Message, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode HITL payload: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return fmt.Errorf("encode HITL payload: %w", err)
	}
	msg.SetMeta(HITLExtensionURI, raw)
	if !slices.Contains(msg.Extensions, HITLExtensionURI) {
		msg.Extensions = append(msg.Extensions, HITLExtensionURI)
	}
	return nil
}

func hitlPayload(msg *a2apkg.Message) (map[string]any, bool) {
	if msg == nil || !slices.Contains(msg.Extensions, HITLExtensionURI) || msg.Metadata == nil {
		return nil, false
	}
	raw, ok := msg.Metadata[HITLExtensionURI].(map[string]any)
	return raw, ok
}

func decodeHITLPayload(raw map[string]any, target any) error {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}
