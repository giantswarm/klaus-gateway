package channels

import (
	"strings"
	"testing"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// askUserPrompt builds the input-required status message kagent emits for an
// ask_user question with the HITL extension activated: the agent's hint as
// text, the typed request as the extension payload.
func askUserPrompt(t *testing.T) *a2apkg.Message {
	t.Helper()
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("How would you like me to proceed?"))
	require.NoError(t, pkga2a.AttachHITL(msg, pkga2a.AskUserRequest{
		Type: pkga2a.HITLTypeAskUserRequest,
		ID:   "adk-123",
		Questions: []pkga2a.HITLQuestion{{
			Question: "How would you like me to proceed?",
			Choices:  []string{"Investigate an issue", "Health check", "Explore tools"},
		}},
	}))
	return msg
}

// approvalPrompt builds the input-required status message for two tool calls
// awaiting approval together.
func approvalPrompt(t *testing.T) *a2apkg.Message {
	t.Helper()
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("Delete the pod?"))
	require.NoError(t, pkga2a.AttachHITL(msg, pkga2a.ToolApprovalRequest{
		Type: pkga2a.HITLTypeToolApprovalRequest,
		Hint: "Delete the pod?",
		Tools: []pkga2a.HITLTool{
			{ID: "approval-1", CallID: "toolu_1", Name: "kubectl_delete", Args: map[string]any{"pod": "web-1"}},
			{ID: "approval-2", CallID: "toolu_2", Name: "kubectl_delete", Args: map[string]any{"pod": "web-2"}},
		},
	}))
	return msg
}

func TestParseHitlPrompt_AskUser(t *testing.T) {
	got := parseHitlPrompt(askUserPrompt(t))
	require.NotNil(t, got)
	require.True(t, got.IsAskUser())
	require.Equal(t, "adk-123", got.OriginalCallID)
	require.Len(t, got.Questions, 1)
	require.Equal(t, "How would you like me to proceed?", got.Questions[0].Question)
	require.False(t, got.Questions[0].Multiple)
	require.Equal(t, []string{"Investigate an issue", "Health check", "Explore tools"}, got.Questions[0].Choices)
}

func TestParseHitlPrompt_ToolApproval(t *testing.T) {
	got := parseHitlPrompt(approvalPrompt(t))
	require.NotNil(t, got)
	require.False(t, got.IsAskUser())
	require.Equal(t, "kubectl_delete", got.ToolName)
	require.Equal(t, "toolu_1", got.OriginalCallID)
	require.Equal(t, "Delete the pod?", got.Hint)
	require.Equal(t, map[string]any{"pod": "web-1"}, got.Args)
	require.Len(t, got.Tools, 2, "every call the decision covers is kept")
	require.Equal(t, "approval-2", got.Tools[1].ID)
}

func TestParseHitlPrompt_IgnoresPlainText(t *testing.T) {
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("approve?"))
	require.Nil(t, parseHitlPrompt(msg))
}

func TestParseHitlPrompt_IgnoresUndeclaredPayload(t *testing.T) {
	// The payload sits in the metadata but the message does not declare the
	// extension: not a HITL request.
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("approve?"))
	msg.SetMeta(pkga2a.HITLExtensionURI, map[string]any{"type": pkga2a.HITLTypeToolApprovalRequest, "tools": []any{map[string]any{"id": "x"}}})
	require.Nil(t, parseHitlPrompt(msg))
}

// The decision's response payload is built against the request the task is
// paused on, so the ids the runtime correlates with come from the request,
// never from the channel.
func TestHitlResponse_AskUserAnswers(t *testing.T) {
	request, err := pkga2a.ParseHITLRequest(askUserPrompt(t))
	require.NoError(t, err)
	got, ok := hitlResponse(request, &HitlDecision{Type: DecisionApprove, AskUserAnswers: [][]string{{"Health check"}}}).(pkga2a.AskUserResponse)
	require.True(t, ok)
	require.Equal(t, pkga2a.HITLTypeAskUserResponse, got.Type)
	require.Equal(t, "adk-123", got.ID, "the answer correlates with the request id")
	require.Equal(t, []pkga2a.AskUserAnswer{{Answer: []string{"Health check"}}}, got.Answers)
}

func TestHitlResponse_ApprovalCoversEveryTool(t *testing.T) {
	request, err := pkga2a.ParseHITLRequest(approvalPrompt(t))
	require.NoError(t, err)

	approved, ok := hitlResponse(request, &HitlDecision{Type: DecisionApprove}).(pkga2a.ToolApprovalResponse)
	require.True(t, ok)
	require.Equal(t, pkga2a.HITLTypeToolApprovalResponse, approved.Type)
	require.Equal(t, []pkga2a.ToolApproval{{ID: "approval-1", Approved: true}, {ID: "approval-2", Approved: true}}, approved.Approvals)

	rejected, _ := hitlResponse(request, &HitlDecision{Type: DecisionReject, RejectionReason: "too risky"}).(pkga2a.ToolApprovalResponse)
	require.Equal(t, []pkga2a.ToolApproval{
		{ID: "approval-1", Approved: false, RejectionReason: "too risky"},
		{ID: "approval-2", Approved: false, RejectionReason: "too risky"},
	}, rejected.Approvals)
}

// A nested request (propagated from a sub-agent) is decided on the child's
// tools, under the child's ids.
func TestHitlResponse_NestedRequestDecidesTheChildTools(t *testing.T) {
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("child asks"))
	require.NoError(t, pkga2a.AttachHITL(msg, pkga2a.ToolApprovalRequest{
		Type:  pkga2a.HITLTypeToolApprovalRequest,
		Tools: []pkga2a.HITLTool{{ID: "parent-1", Name: "sub_agent"}},
		Nested: &pkga2a.NestedHITLRequest{SubagentName: "child", TaskID: "ct", Tools: []pkga2a.HITLTool{
			{ID: "child-1", Name: "rm"},
		}},
	}))
	request, err := pkga2a.ParseHITLRequest(msg)
	require.NoError(t, err)
	got, _ := hitlResponse(request, &HitlDecision{Type: DecisionApprove}).(pkga2a.ToolApprovalResponse)
	require.Equal(t, []pkga2a.ToolApproval{{ID: "child-1", Approved: true}}, got.Approvals)
	require.Equal(t, "rm", parseHitlPrompt(msg).ToolName, "the prompt shows the child's tool")
}

// A decision travels as the extension payload; its parts are only the
// human-readable label.
func TestBuildInboundParts_DecisionIsLabelOnly(t *testing.T) {
	parts := buildInboundParts(InboundMessage{Text: "Health check", Decision: &HitlDecision{Type: DecisionApprove}})
	require.Len(t, parts, 1)
	require.Equal(t, "Health check", parts[0].Text())

	parts = buildInboundParts(InboundMessage{Decision: &HitlDecision{Type: DecisionReject, RejectionReason: "too risky"}})
	require.Len(t, parts, 1)
	require.Equal(t, "reject", parts[0].Text(), "a decision without text is labelled by its type")
}

func TestBuildInboundParts_PlainTextWithoutDecision(t *testing.T) {
	parts := buildInboundParts(InboundMessage{Text: "hello"})
	require.Len(t, parts, 1)
	require.Equal(t, "hello", parts[0].Text())
}

func TestBuildInboundParts_TextAndAttachment(t *testing.T) {
	msg := InboundMessage{
		Text: "look at this",
		Attachments: []Attachment{
			{Filename: "shot.png", ContentType: "image/png", Bytes: []byte{0x89, 0x50}},
		},
	}
	parts := buildInboundParts(msg)
	require.Len(t, parts, 2)
	require.Equal(t, "look at this", parts[0].Text())
	require.Equal(t, []byte{0x89, 0x50}, parts[1].Raw())
	require.Equal(t, "shot.png", parts[1].Filename)
	require.Equal(t, "image/png", parts[1].MediaType)
}

func TestBuildInboundParts_TextAttachmentBecomesTextPart(t *testing.T) {
	// A text/* attachment must be forwarded as a text part, not a binary file
	// part: the model backend rejects a text/plain inline_data blob.
	msg := InboundMessage{
		Text: "review this",
		Attachments: []Attachment{
			{Filename: "ingress.yaml", ContentType: "text/plain", Bytes: []byte("kind: Ingress")},
		},
	}
	parts := buildInboundParts(msg)
	require.Len(t, parts, 2)
	require.Equal(t, "review this", parts[0].Text())
	require.Nil(t, parts[1].Raw(), "text attachment must not be a raw/file part")
	require.Contains(t, parts[1].Text(), "ingress.yaml")
	require.Contains(t, parts[1].Text(), "kind: Ingress")
}

func TestBuildInboundParts_StructuredTextAttachment(t *testing.T) {
	msg := InboundMessage{
		Attachments: []Attachment{
			{Filename: "data.json", ContentType: mediaTypeJSON, Bytes: []byte(`{"a":1}`)},
		},
	}
	parts := buildInboundParts(msg)
	require.Len(t, parts, 1)
	require.Nil(t, parts[0].Raw())
	require.Contains(t, parts[0].Text(), `{"a":1}`)
}

func TestIsTextualMediaType(t *testing.T) {
	for _, mt := range []string{"text/plain", "text/plain; charset=utf-8", "text/yaml", mediaTypeJSON, "application/x-yaml", "application/vnd.api+json"} {
		require.True(t, isTextualMediaType(mt), mt)
	}
	for _, mt := range []string{"image/png", "application/pdf", "application/octet-stream", ""} {
		require.False(t, isTextualMediaType(mt), mt)
	}
}

func TestBuildInboundParts_AttachmentOnly_NoEmptyTextPart(t *testing.T) {
	msg := InboundMessage{
		Attachments: []Attachment{
			{Filename: "shot.png", ContentType: "image/png", Bytes: []byte{0x1}},
		},
	}
	parts := buildInboundParts(msg)
	require.Len(t, parts, 1)
	require.Equal(t, []byte{0x1}, parts[0].Raw())
}

func TestBuildInboundParts_SkipsAttachmentWithoutBytes(t *testing.T) {
	msg := InboundMessage{
		Text: "caption",
		Attachments: []Attachment{
			{Filename: "failed.png", ContentType: "image/png"}, // no Bytes: download failed
		},
	}
	parts := buildInboundParts(msg)
	require.Len(t, parts, 1)
	require.Equal(t, "caption", parts[0].Text())
}

// TestTextAttachment_FenceOutgrowsContentFences pins the anti-breakout fence:
// a file whose content itself contains ``` fences must stay fully inside the
// quoted block, so the wrapper fence is always longer than any run inside.
func TestTextAttachment_FenceOutgrowsContentFences(t *testing.T) {
	content := "# readme\n```go\ncode\n```\nrest"
	got := textAttachment(Attachment{Filename: "README.md", Bytes: []byte(content)})
	require.True(t, strings.HasPrefix(got, "Attached file `README.md`:\n````\n"), got)
	require.True(t, strings.HasSuffix(got, "\n````"), got)
	require.Contains(t, got, content)
}

func TestTextAttachment_PlainContentUsesMinimalFence(t *testing.T) {
	got := textAttachment(Attachment{Filename: "a.txt", Bytes: []byte("plain")})
	require.Equal(t, "Attached file `a.txt`:\n```\nplain\n```", got)
}
