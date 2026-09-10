package channels

import (
	"strings"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// Part metadata keys kagent sets on the DataParts of tool activity. Both the
// kagent_ and adk_ prefixes appear in the wild; we accept either. A
// long-running function_call is the runtime's own confirmation bookkeeping,
// never tool activity to show.
const (
	mdTypeKagent           = "kagent_type"
	mdTypeADK              = "adk_type"
	mdLongRunningKagent    = "kagent_is_long_running"
	mdLongRunningADK       = "adk_is_long_running"
	mdTypeFunctionCall     = "function_call"
	mdTypeFunctionResponse = "function_response"
)

// Event/message-level metadata keys kagent sets for token usage. Both prefixes
// appear in the wild; we accept either. The value is a flat object with the
// camelCase field names below.
const (
	mdUsageKagent = "kagent_usage_metadata"
	mdUsageADK    = "adk_usage_metadata"

	mdPartialKagent = "kagent_partial"
	mdPartialADK    = "adk_partial"

	usagePromptTokens     = "promptTokenCount"
	usageCompletionTokens = "candidatesTokenCount"
	usageTotalTokens      = "totalTokenCount"
)

// buildInboundParts builds the A2A message parts for an outbound user turn: a
// text part (when there is text) plus one part per downloaded attachment,
// falling back to a single empty text part so the A2A message stays
// well-formed. A HITL decision is carried by the message's extension payload,
// not by a part; its parts are a human-readable label of the decision, so the
// conversation's history reads as a dialogue.
func buildInboundParts(msg InboundMessage) []*a2apkg.Part {
	if msg.Decision != nil {
		label := msg.Text
		if label == "" {
			label = msg.Decision.Type
		}
		return []*a2apkg.Part{a2apkg.NewTextPart(withAuthor(msg.Author, label))}
	}
	var parts []*a2apkg.Part
	if text := withAuthor(msg.Author, msg.Text); text != "" {
		parts = append(parts, a2apkg.NewTextPart(text))
	}
	parts = append(parts, attachmentParts(msg.Attachments)...)
	if len(parts) == 0 {
		// An attachment-only message whose downloads all failed leaves no
		// content; an empty text part keeps the A2A message well-formed.
		parts = append(parts, a2apkg.NewTextPart(""))
	}
	return parts
}

// attachmentParts builds an A2A part per attachment that has downloaded bytes.
// A text attachment (yaml, json, source, …) becomes a text part carrying its
// content: model backends reject a text/* binary blob ("Not supported yet:
// inline_data=Blob(… mime_type='text/plain')"), and a text file is only useful
// to the agent as readable text anyway. Binary attachments (images, PDFs) stay
// as file parts for a vision-capable model. Attachments without bytes (a failed
// download) are skipped.
func attachmentParts(attachments []Attachment) []*a2apkg.Part {
	parts := make([]*a2apkg.Part, 0, len(attachments))
	for _, att := range attachments {
		if len(att.Bytes) == 0 {
			continue
		}
		if isTextualMediaType(att.ContentType) {
			parts = append(parts, a2apkg.NewTextPart(textAttachment(att)))
			continue
		}
		part := a2apkg.NewRawPart(att.Bytes)
		part.Filename = att.Filename
		part.MediaType = att.ContentType
		parts = append(parts, part)
	}
	return parts
}

// textAttachment renders a text attachment as a labeled, fenced block so the
// agent sees both the filename and the file's content as readable text. The
// fence is one backtick longer than the longest backtick run in the content,
// so a file that itself contains ``` fences (any markdown with code blocks)
// cannot close the block early and leak its remainder outside the quoted
// context.
func textAttachment(att Attachment) string {
	name := att.Filename
	if name == "" {
		name = "attachment"
	}
	fence := strings.Repeat("`", max(3, longestBacktickRun(att.Bytes)+1))
	return "Attached file `" + name + "`:\n" + fence + "\n" + string(att.Bytes) + "\n" + fence
}

// longestBacktickRun returns the length of the longest consecutive run of
// backticks in b.
func longestBacktickRun(b []byte) int {
	longest, run := 0, 0
	for _, c := range b {
		if c != '`' {
			run = 0
			continue
		}
		if run++; run > longest {
			longest = run
		}
	}
	return longest
}

// mediaTypeJSON is the JSON media type, named because the literal recurs
// across the textual-type switch and its tests.
const mediaTypeJSON = "application/json"

// isTextualMediaType reports whether a media type is human-readable text that
// should reach the agent as a text part rather than a binary file blob. It
// ignores any charset parameter and matches text/*, the common structured text
// types, and the +json/+xml/+yaml structured suffixes.
func isTextualMediaType(mediaType string) bool {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case mediaTypeJSON, "application/xml", "application/yaml",
		"application/x-yaml", "application/x-yml", "application/toml",
		"application/x-sh", "application/javascript", "application/x-ndjson",
		"application/csv":
		return true
	}
	return strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml") || strings.HasSuffix(mt, "+yaml")
}

// withAuthor prefixes text with the real author when the turn runs under a
// delegated identity (a shared thread session acting as its initiator), so the
// agent sees who actually spoke. Returns text unchanged when author is empty.
func withAuthor(author, text string) string {
	if author == "" {
		return text
	}
	return "[message from " + author + "]\n" + text
}

// parseHitlPrompt extracts the structured request from an input-required A2A
// status message: the HITL extension payload kagent attaches when the client
// requested the extension. Returns nil when the message carries none (a
// plain-text prompt, or a malformed payload — the text is still rendered).
func parseHitlPrompt(msg *a2apkg.Message) *HitlPrompt {
	request, err := pkga2a.ParseHITLRequest(msg)
	if err != nil || request == nil {
		return nil
	}
	if ask := request.AskUser; ask != nil {
		prompt := &HitlPrompt{ToolName: AskUserToolName, OriginalCallID: ask.ResponseID()}
		for _, q := range ask.Questions {
			prompt.Questions = append(prompt.Questions, HitlQuestion{Question: q.Question, Choices: q.Choices, Multiple: q.Multiple})
		}
		return prompt
	}
	approval := request.ToolApproval
	prompt := &HitlPrompt{Hint: approval.Hint}
	for _, tool := range approval.DecidedTools() {
		prompt.Tools = append(prompt.Tools, HitlTool{ID: tool.ID, CallID: tool.CallID, Name: tool.Name, Args: tool.Args})
	}
	if len(prompt.Tools) > 0 {
		first := prompt.Tools[0]
		prompt.ToolName, prompt.OriginalCallID, prompt.Args = first.Name, first.CallID, first.Args
	}
	return prompt
}

// isConfirmationPart reports whether a DataPart's metadata marks it as a
// long-running function_call: the runtime's confirmation bookkeeping, which
// is surfaced through the input-required prompt rather than as tool activity.
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

// hasFunctionCallPart reports whether any of parts is a function_call DataPart.
func hasFunctionCallPart(parts a2apkg.ContentParts) bool {
	for _, p := range parts {
		if p == nil {
			continue
		}
		if typ, _ := firstString(p.Metadata, mdTypeKagent, mdTypeADK); typ == mdTypeFunctionCall {
			return true
		}
	}
	return false
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
	if len(p.Tools) > 1 {
		names := make([]string, 0, len(p.Tools))
		for _, t := range p.Tools {
			names = append(names, t.Name)
		}
		return strings.Join(names, ", ")
	}
	return p.ToolName
}

// isPartialMeta reports whether metadata marks the event as a partial
// (streaming) chunk. Partial events mirror the usage metadata of the LLM call
// they belong to, so counting usage on them would tally the same call multiple
// times; kagent's own task store filters on the same keys.
func isPartialMeta(md map[string]any) bool {
	partial, _ := firstBool(md, mdPartialKagent, mdPartialADK)
	return partial
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
		InputTokens:  mdInt(raw, usagePromptTokens),
		OutputTokens: mdInt(raw, usageCompletionTokens),
		TotalTokens:  mdInt(raw, usageTotalTokens),
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
