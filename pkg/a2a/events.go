package a2a

import (
	"strings"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

// workingEvent emits a working-state status update with an optional message.
func workingEvent(reqCtx *a2asrv.RequestContext, msg string) *a2a.TaskStatusUpdateEvent {
	var a2aMsg *a2a.Message
	if msg != "" {
		a2aMsg = &a2a.Message{
			Role:  a2a.MessageRoleAgent,
			Parts: []a2a.Part{a2a.TextPart{Text: msg}},
		}
	}
	return a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateWorking, a2aMsg)
}

// artifactEvent emits the first artifact chunk for the turn result.
func artifactEvent(reqCtx *a2asrv.RequestContext, text string) *a2a.TaskArtifactUpdateEvent {
	return a2a.NewArtifactEvent(reqCtx, a2a.TextPart{Text: text})
}

// artifactUpdateEvent appends subsequent chunks to an existing artifact.
func artifactUpdateEvent(reqCtx *a2asrv.RequestContext, artifactID a2a.ArtifactID, text string) *a2a.TaskArtifactUpdateEvent {
	return a2a.NewArtifactUpdateEvent(reqCtx, artifactID, a2a.TextPart{Text: text})
}

// completedEvent emits the final completed-state status update.
func completedEvent(reqCtx *a2asrv.RequestContext) *a2a.TaskStatusUpdateEvent {
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCompleted, nil)
	ev.Final = true
	return ev
}

// failedEvent emits a final failed-state status update with an error description.
func failedEvent(reqCtx *a2asrv.RequestContext, errText string) *a2a.TaskStatusUpdateEvent {
	msg := &a2a.Message{
		Role:  a2a.MessageRoleAgent,
		Parts: []a2a.Part{a2a.TextPart{Text: errText}},
	}
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateFailed, msg)
	ev.Final = true
	return ev
}

// canceledEvent emits a final canceled-state status update.
func canceledEvent(reqCtx *a2asrv.RequestContext) *a2a.TaskStatusUpdateEvent {
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCanceled, nil)
	ev.Final = true
	return ev
}

// inputRequiredEvent emits a final input-required status update with an optional message.
func inputRequiredEvent(reqCtx *a2asrv.RequestContext, msg string) *a2a.TaskStatusUpdateEvent {
	var a2aMsg *a2a.Message
	if msg != "" {
		a2aMsg = &a2a.Message{
			Role:  a2a.MessageRoleAgent,
			Parts: []a2a.Part{a2a.TextPart{Text: msg}},
		}
	}
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateInputRequired, a2aMsg)
	ev.Final = true
	return ev
}

// authRequiredEvent emits a final auth-required status update with an optional message.
func authRequiredEvent(reqCtx *a2asrv.RequestContext, msg string) *a2a.TaskStatusUpdateEvent {
	var a2aMsg *a2a.Message
	if msg != "" {
		a2aMsg = &a2a.Message{
			Role:  a2a.MessageRoleAgent,
			Parts: []a2a.Part{a2a.TextPart{Text: msg}},
		}
	}
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateAuthRequired, a2aMsg)
	ev.Final = true
	return ev
}

// rejectedEvent emits a final rejected-state status update with an optional message.
func rejectedEvent(reqCtx *a2asrv.RequestContext, msg string) *a2a.TaskStatusUpdateEvent {
	var a2aMsg *a2a.Message
	if msg != "" {
		a2aMsg = &a2a.Message{
			Role:  a2a.MessageRoleAgent,
			Parts: []a2a.Part{a2a.TextPart{Text: msg}},
		}
	}
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateRejected, a2aMsg)
	ev.Final = true
	return ev
}

// extractText collects all text parts from a message into a single string.
func extractText(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	return extractTextFromParts(msg.Parts)
}

// extractTextFromParts collects text from a slice of a2a.Part.
func extractTextFromParts(parts []a2a.Part) string {
	var sb strings.Builder
	for _, p := range parts {
		if tp, ok := p.(a2a.TextPart); ok {
			sb.WriteString(tp.Text)
		}
	}
	return sb.String()
}
