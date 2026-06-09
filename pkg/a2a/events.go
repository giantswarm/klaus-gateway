package a2a

import (
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func workingEvent(execCtx *a2asrv.ExecutorContext, msg string) *a2a.TaskStatusUpdateEvent {
	var a2aMsg *a2a.Message
	if msg != "" {
		a2aMsg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
	}
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, a2aMsg)
}

func artifactEvent(execCtx *a2asrv.ExecutorContext, text string) *a2a.TaskArtifactUpdateEvent {
	return a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(text))
}

func artifactUpdateEvent(execCtx *a2asrv.ExecutorContext, artifactID a2a.ArtifactID, text string) *a2a.TaskArtifactUpdateEvent {
	return a2a.NewArtifactUpdateEvent(execCtx, artifactID, a2a.NewTextPart(text))
}

func completedEvent(execCtx *a2asrv.ExecutorContext) *a2a.TaskStatusUpdateEvent {
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil)
}

func failedEvent(execCtx *a2asrv.ExecutorContext, errText string) *a2a.TaskStatusUpdateEvent {
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed,
		a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(errText)))
}

func canceledEvent(execCtx *a2asrv.ExecutorContext) *a2a.TaskStatusUpdateEvent {
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil)
}

func inputRequiredEvent(execCtx *a2asrv.ExecutorContext, msg string) *a2a.TaskStatusUpdateEvent {
	var a2aMsg *a2a.Message
	if msg != "" {
		a2aMsg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
	}
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, a2aMsg)
}

func authRequiredEvent(execCtx *a2asrv.ExecutorContext, msg string) *a2a.TaskStatusUpdateEvent {
	var a2aMsg *a2a.Message
	if msg != "" {
		a2aMsg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
	}
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateAuthRequired, a2aMsg)
}

func rejectedEvent(execCtx *a2asrv.ExecutorContext, msg string) *a2a.TaskStatusUpdateEvent {
	var a2aMsg *a2a.Message
	if msg != "" {
		a2aMsg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
	}
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateRejected, a2aMsg)
}

func extractText(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	return extractTextFromParts(msg.Parts)
}

func extractTextFromParts(parts a2a.ContentParts) string {
	var sb strings.Builder
	for _, p := range parts {
		if p != nil {
			sb.WriteString(p.Text())
		}
	}
	return sb.String()
}
