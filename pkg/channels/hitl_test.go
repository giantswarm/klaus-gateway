package channels

import (
	"testing"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"
)

// askUserConfirmationPart builds the DataPart kagent emits for an ask_user
// input-required prompt, mirroring the real wire payload.
func askUserConfirmationPart() *a2apkg.Part {
	p := a2apkg.NewDataPart(map[string]any{
		"name": "adk_request_confirmation",
		"id":   "adk-123",
		"args": map[string]any{
			"originalFunctionCall": map[string]any{
				"name": "ask_user",
				"id":   "toolu_abc",
				"args": map[string]any{
					"questions": []any{
						map[string]any{
							"question": "How would you like me to proceed?",
							"multiple": false,
							"choices":  []any{"Investigate an issue", "Health check", "Explore tools"},
						},
					},
				},
			},
			"toolConfirmation": map[string]any{"confirmed": false, "hint": "How would you like me to proceed?"},
		},
	})
	p.Metadata = map[string]any{"kagent_type": "function_call", "kagent_is_long_running": true}
	return p
}

func TestParseHitlPrompt_AskUser(t *testing.T) {
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, askUserConfirmationPart())

	got := parseHitlPrompt(msg)
	require.NotNil(t, got)
	require.True(t, got.IsAskUser())
	require.Equal(t, "toolu_abc", got.OriginalCallID)
	require.Len(t, got.Questions, 1)
	require.Equal(t, "How would you like me to proceed?", got.Questions[0].Question)
	require.False(t, got.Questions[0].Multiple)
	require.Equal(t, []string{"Investigate an issue", "Health check", "Explore tools"}, got.Questions[0].Choices)
}

func TestParseHitlPrompt_IgnoresPlainText(t *testing.T) {
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, a2apkg.NewTextPart("approve?"))
	require.Nil(t, parseHitlPrompt(msg))
}

func TestParseHitlPrompt_IgnoresNonLongRunningDataPart(t *testing.T) {
	p := a2apkg.NewDataPart(map[string]any{"name": "adk_request_confirmation"})
	p.Metadata = map[string]any{"kagent_type": "function_call"} // missing is_long_running
	msg := a2apkg.NewMessage(a2apkg.MessageRoleAgent, p)
	require.Nil(t, parseHitlPrompt(msg))
}

func TestBuildInboundParts_AskUserAnswers(t *testing.T) {
	msg := InboundMessage{
		Text: "Health check",
		Decision: &HitlDecision{
			Type:           DecisionApprove,
			AskUserAnswers: [][]string{{"Health check"}},
		},
	}
	parts := buildInboundParts(msg)
	require.Len(t, parts, 2)

	data, ok := parts[0].Data().(map[string]any)
	require.True(t, ok)
	require.Equal(t, "approve", data["decision_type"])
	answers, ok := data["ask_user_answers"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, answers, 1)
	require.Equal(t, []string{"Health check"}, answers[0]["answer"])

	require.Equal(t, "Health check", parts[1].Text())
}

func TestBuildInboundParts_RejectWithReason(t *testing.T) {
	msg := InboundMessage{
		Decision: &HitlDecision{Type: DecisionReject, RejectionReason: "too risky"},
	}
	parts := buildInboundParts(msg)
	data, ok := parts[0].Data().(map[string]any)
	require.True(t, ok)
	require.Equal(t, "reject", data["decision_type"])
	require.Equal(t, "too risky", data["rejection_reason"])
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
			{Filename: "data.json", ContentType: "application/json", Bytes: []byte(`{"a":1}`)},
		},
	}
	parts := buildInboundParts(msg)
	require.Len(t, parts, 1)
	require.Nil(t, parts[0].Raw())
	require.Contains(t, parts[0].Text(), `{"a":1}`)
}

func TestIsTextualMediaType(t *testing.T) {
	for _, mt := range []string{"text/plain", "text/plain; charset=utf-8", "text/yaml", "application/json", "application/x-yaml", "application/vnd.api+json"} {
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
