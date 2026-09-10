package channels

import pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"

// Human-in-the-loop (HITL) types shared between the A2A facade and channel
// adapters.
//
// kagent surfaces a tool that requires approval (or the built-in ask_user
// tool) as an A2A task in the input-required state. With the HITL extension
// requested on the call, the status message carries a typed request payload
// under the extension URI in its metadata — a tool_approval_request naming the
// tool calls to decide on, or an ask_user_request with the questions — and its
// text part carries the agent's hint.
//
// The decision travels back as the matching typed response payload on a user
// message that MUST carry the paused task's id, so the paused task resumes in
// place. The facade builds that payload against the request the task is
// paused on; channels only produce a HitlDecision.

// HitlPrompt is the structured request parsed from an input-required A2A
// status message. It is attached to a DeltaPrompt.
type HitlPrompt struct {
	// ToolName is the name of the tool awaiting approval — the first one when
	// several calls are decided together — or "ask_user" for a question.
	ToolName string
	// Hint is the agent's human-readable hint for a tool approval.
	Hint string
	// OriginalCallID is the model's call id of the first tool awaiting approval.
	OriginalCallID string
	// Args is the arguments of the first tool awaiting approval, for rendering.
	Args map[string]any
	// Tools lists every tool call the decision covers; a decision approves or
	// rejects all of them together.
	Tools []HitlTool
	// Questions is populated only when ToolName == "ask_user".
	Questions []HitlQuestion
}

// HitlTool is one tool call awaiting approval.
type HitlTool struct {
	// ID is the approval id the decision answers with.
	ID string
	// CallID is the model's call id.
	CallID string
	Name   string
	Args   map[string]any
}

// IsAskUser reports whether this prompt is the built-in ask_user question tool.
func (p *HitlPrompt) IsAskUser() bool {
	return p != nil && p.ToolName == AskUserToolName
}

// AskUserToolName is the kagent built-in question tool name.
const AskUserToolName = pkga2a.AskUserToolName

// HitlQuestion is a single question in an ask_user call.
type HitlQuestion struct {
	Question string
	Choices  []string
	Multiple bool // true = multi-select allowed
}

// HitlDecision is the user's structured answer to a HitlPrompt, sent back as
// the HITL response payload on the message that resumes the paused task.
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

// HITL decision types.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// hitlResponse builds the typed HITL response payload for a decision on the
// request a task is paused on: one approval per requested tool (all approved
// or all rejected together, the reason on a rejection), or the positional
// answers of an ask_user request under its correlation id.
func hitlResponse(request *pkga2a.HITLRequest, decision *HitlDecision) any {
	if request.AskUser != nil {
		answers := make([]pkga2a.AskUserAnswer, 0, len(decision.AskUserAnswers))
		for _, a := range decision.AskUserAnswers {
			answers = append(answers, pkga2a.AskUserAnswer{Answer: a})
		}
		return pkga2a.AskUserResponse{Type: pkga2a.HITLTypeAskUserResponse, ID: request.AskUser.ResponseID(), Answers: answers}
	}
	approved := decision.Type == DecisionApprove
	tools := request.ToolApproval.DecidedTools()
	approvals := make([]pkga2a.ToolApproval, 0, len(tools))
	for _, tool := range tools {
		approval := pkga2a.ToolApproval{ID: tool.ID, Approved: approved}
		if !approved {
			approval.RejectionReason = decision.RejectionReason
		}
		approvals = append(approvals, approval)
	}
	return pkga2a.ToolApprovalResponse{Type: pkga2a.HITLTypeToolApprovalResponse, Approvals: approvals}
}
