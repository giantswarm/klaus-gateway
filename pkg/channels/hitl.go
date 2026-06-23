package channels

// Human-in-the-loop (HITL) types shared between the A2A facade and channel
// adapters.
//
// kagent surfaces a tool that requires approval (or the built-in ask_user
// tool) as an A2A task in the input-required state. The status message carries
// a structured DataPart (not text) describing an adk_request_confirmation:
//
//	{"name":"adk_request_confirmation",
//	 "args":{"originalFunctionCall":{"name":...,"args":...,"id":...},
//	         "toolConfirmation":{"hint":...,"confirmed":false}}}
//
// with part metadata kagent_type=function_call and kagent_is_long_running=true.
//
// The decision is returned as a DataPart on a user message that MUST carry the
// paused taskId. kagent only resolves the pending confirmation from a DataPart
// — a plain text reply leaves the tool call dangling, which corrupts the model
// history (tool_use without tool_result). See kagent
// docs/architecture/human-in-the-loop.md for the wire contract.

// HitlPrompt is the structured approval request parsed from an input-required
// A2A status message. It is attached to a DeltaPrompt.
type HitlPrompt struct {
	// ToolName is the originalFunctionCall name, e.g. "ask_user" or "delete_file".
	ToolName string
	// Hint is the human-readable toolConfirmation hint.
	Hint string
	// OriginalCallID is the originalFunctionCall id, used as the key for batch
	// decisions.
	OriginalCallID string
	// Args is the originalFunctionCall args, for rendering generic approval tools.
	Args map[string]any
	// Questions is populated only when ToolName == "ask_user".
	Questions []HitlQuestion
}

// IsAskUser reports whether this prompt is the built-in ask_user question tool.
func (p *HitlPrompt) IsAskUser() bool {
	return p != nil && p.ToolName == AskUserToolName
}

// AskUserToolName is the kagent built-in question tool name.
const AskUserToolName = "ask_user"

// HitlQuestion is a single question in an ask_user call.
type HitlQuestion struct {
	Question string
	Choices  []string
	Multiple bool // true = multi-select allowed
}

// HitlDecision is the user's structured answer to a HitlPrompt, sent back as an
// A2A DataPart to resume the paused task.
type HitlDecision struct {
	// Type is "approve" or "reject".
	Type string
	// AskUserAnswers is positional, 1:1 with HitlPrompt.Questions. Each entry is
	// the selected (or typed) answer label(s) for that question. Only set for
	// ask_user prompts.
	AskUserAnswers [][]string
	// RejectionReason is an optional free-text reason attached to a reject.
	RejectionReason string
}

// HITL decision type constants matching the kagent wire protocol.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)
