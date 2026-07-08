package channels

import (
	"strings"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
)

// Part metadata keys kagent sets on an adk_request_confirmation DataPart.
// Both the kagent_ and adk_ prefixes appear in the wild; we accept either.
const (
	mdTypeKagent           = "kagent_type"
	mdTypeADK              = "adk_type"
	mdLongRunningKagent    = "kagent_is_long_running"
	mdLongRunningADK       = "adk_is_long_running"
	mdTypeFunctionCall     = "function_call"
	mdTypeFunctionResponse = "function_response"
	confirmationToolName   = "adk_request_confirmation"
)

// Event/message-level metadata keys kagent sets for token usage. Both prefixes
// appear in the wild; we accept either. The value is a flat object with the
// camelCase field names below.
const (
	mdUsageKagent = "kagent_usage_metadata"
	mdUsageADK    = "adk_usage_metadata"

	usagePromptTokens     = "promptTokenCount"
	usageCompletionTokens = "candidatesTokenCount"
	usageTotalTokens      = "totalTokenCount"
)

// buildInboundParts builds the A2A message parts for an outbound user turn.
// When msg.Decision is set it emits a structured HITL decision DataPart plus a
// human-readable text label; otherwise a single text part.
func buildInboundParts(msg InboundMessage) []*a2apkg.Part {
	if msg.Decision == nil {
		return []*a2apkg.Part{a2apkg.NewTextPart(msg.Text)}
	}

	data := map[string]any{"decision_type": msg.Decision.Type}
	if len(msg.Decision.AskUserAnswers) > 0 {
		answers := make([]map[string]any, 0, len(msg.Decision.AskUserAnswers))
		for _, a := range msg.Decision.AskUserAnswers {
			answers = append(answers, map[string]any{"answer": a})
		}
		data["ask_user_answers"] = answers
	}
	if msg.Decision.Type == DecisionReject && msg.Decision.RejectionReason != "" {
		data["rejection_reason"] = msg.Decision.RejectionReason
	}

	label := msg.Text
	if label == "" {
		label = msg.Decision.Type
	}
	return []*a2apkg.Part{a2apkg.NewDataPart(data), a2apkg.NewTextPart(label)}
}

// parseHitlPrompt extracts a structured approval request from an
// input-required A2A status message. Returns nil when the message carries no
// adk_request_confirmation DataPart (e.g. a plain-text input-required prompt).
func parseHitlPrompt(msg *a2apkg.Message) *HitlPrompt {
	if msg == nil {
		return nil
	}
	for _, p := range msg.Parts {
		if p == nil || !isConfirmationPart(p.Metadata) {
			continue
		}
		data, ok := p.Data().(map[string]any)
		if !ok {
			continue
		}
		if name, _ := data["name"].(string); name != confirmationToolName {
			continue
		}
		args, _ := data["args"].(map[string]any)
		ofc, _ := args["originalFunctionCall"].(map[string]any)
		if ofc == nil {
			continue
		}

		prompt := &HitlPrompt{}
		prompt.ToolName, _ = ofc["name"].(string)
		prompt.OriginalCallID, _ = ofc["id"].(string)
		prompt.Args, _ = ofc["args"].(map[string]any)
		if tc, ok := args["toolConfirmation"].(map[string]any); ok {
			prompt.Hint, _ = tc["hint"].(string)
		}
		if prompt.ToolName == AskUserToolName {
			prompt.Questions = parseAskUserQuestions(prompt.Args)
		}
		return prompt
	}
	return nil
}

// isConfirmationPart reports whether a DataPart's metadata marks it as a
// long-running function_call (i.e. an adk_request_confirmation).
func isConfirmationPart(md map[string]any) bool {
	if md == nil {
		return false
	}
	typ, _ := firstString(md, mdTypeKagent, mdTypeADK)
	if typ != mdTypeFunctionCall {
		return false
	}
	lr, _ := firstBool(md, mdLongRunningKagent, mdLongRunningADK)
	return lr
}

// parseAskUserQuestions extracts the questions array from ask_user args.
func parseAskUserQuestions(args map[string]any) []HitlQuestion {
	raw, ok := args["questions"].([]any)
	if !ok {
		return nil
	}
	var out []HitlQuestion
	for _, q := range raw {
		qm, ok := q.(map[string]any)
		if !ok {
			continue
		}
		hq := HitlQuestion{}
		hq.Question, _ = qm["question"].(string)
		hq.Multiple, _ = qm["multiple"].(bool)
		if choices, ok := qm["choices"].([]any); ok {
			for _, c := range choices {
				if s, ok := c.(string); ok {
					hq.Choices = append(hq.Choices, s)
				}
			}
		}
		out = append(out, hq)
	}
	return out
}

// summary renders a short plain-text description of the prompt, used as a
// fallback render when the input-required status carries no text part.
func (p *HitlPrompt) summary() string {
	if p == nil {
		return ""
	}
	if p.IsAskUser() {
		var b strings.Builder
		for i, q := range p.Questions {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(q.Question)
			if len(q.Choices) > 0 {
				b.WriteString(" [")
				b.WriteString(strings.Join(q.Choices, ", "))
				b.WriteString("]")
			}
		}
		return b.String()
	}
	if p.Hint != "" {
		return p.Hint
	}
	return p.ToolName
}

// parseTurnUsage reads a kagent usage-metadata object from event or message
// metadata. Returns nil when no usage object is present. Every field is
// optional (a provider populates only what it reports).
func parseTurnUsage(md map[string]any) *TurnUsage {
	if md == nil {
		return nil
	}
	var raw map[string]any
	for _, k := range []string{mdUsageKagent, mdUsageADK} {
		if v, ok := md[k].(map[string]any); ok {
			raw = v
			break
		}
	}
	if raw == nil {
		return nil
	}
	return &TurnUsage{
		PromptTokens:     mdInt(raw, usagePromptTokens),
		CompletionTokens: mdInt(raw, usageCompletionTokens),
		TotalTokens:      mdInt(raw, usageTotalTokens),
	}
}

// toolActivityDelta builds a DeltaToolActivity from a DataPart whose metadata
// marks it a function_call or function_response, translating the kagent payload
// into the neutral ToolActivity. Returns a zero delta when the part is not tool
// activity. Content is a short summary (the tool name).
func toolActivityDelta(p *a2apkg.Part) OutboundDelta {
	if p == nil {
		return OutboundDelta{}
	}
	typ, ok := firstString(p.Metadata, mdTypeKagent, mdTypeADK)
	if !ok || (typ != mdTypeFunctionCall && typ != mdTypeFunctionResponse) {
		return OutboundDelta{}
	}
	// A long-running function_call is an adk_request_confirmation (HITL), handled
	// on the input-required path, not surfaced as tool activity.
	if typ == mdTypeFunctionCall && isConfirmationPart(p.Metadata) {
		return OutboundDelta{}
	}
	data, _ := p.Data().(map[string]any)
	tool := &ToolActivity{}
	tool.Name, _ = data["name"].(string)
	tool.CallID, _ = data["id"].(string)
	if typ == mdTypeFunctionResponse {
		tool.Kind = ToolResult
		tool.Response, _ = data["response"].(map[string]any)
	} else {
		tool.Kind = ToolCall
		tool.Args, _ = data["args"].(map[string]any)
	}
	return OutboundDelta{Kind: DeltaToolActivity, Content: tool.Name, Tool: tool}
}

// mdInt reads an integer usage field. A2A event metadata is JSON-decoded, so
// numbers arrive as float64.
func mdInt(md map[string]any, key string) int {
	f, _ := md[key].(float64)
	return int(f)
}

func firstString(md map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := md[k].(string); ok {
			return v, true
		}
	}
	return "", false
}

func firstBool(md map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := md[k].(bool); ok {
			return v, true
		}
	}
	return false, false
}
