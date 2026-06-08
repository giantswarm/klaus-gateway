package a2a

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	a2apkg "github.com/a2aproject/a2a-go/a2a"
	a2aclient "github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	"github.com/giantswarm/klaus-gateway/pkg/kagentapi"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
)

// Resolver maps an inbound message to the lifecycle instance that should handle it.
// The routing.Router satisfies this interface.
type Resolver interface {
	Resolve(ctx context.Context, msg routing.InboundMessage) (lifecycle.InstanceRef, error)
}

// Dialer returns a connected A2A client for the given base URL.
// Clients satisfies this interface.
type Dialer interface {
	For(ctx context.Context, baseURL string) (*a2aclient.Client, error)
}

// KagentPusher persists turn data in kagent.
// kagentapi.Client satisfies this interface.
type KagentPusher interface {
	Enabled() bool
	PushEvent(ctx context.Context, sessionID string, event kagentapi.SessionEvent, auth kagentapi.AuthInfo)
	StoreTask(ctx context.Context, taskID, contextID, userText, agentText string, auth kagentapi.AuthInfo)
}

// authInfoKey is the context key used to pass caller identity from the HTTP
// middleware into the executor. Unexported; set only via WithAuthInfo and read
// only via AuthInfoFromContext.
type authInfoKey struct{}

// WithAuthInfo stores the caller's identity in ctx.
func WithAuthInfo(ctx context.Context, auth kagentapi.AuthInfo) context.Context {
	return context.WithValue(ctx, authInfoKey{}, auth)
}

// AuthInfoFromContext retrieves caller identity previously stored by WithAuthInfo.
// Returns a zero AuthInfo if none is present.
func AuthInfoFromContext(ctx context.Context) kagentapi.AuthInfo {
	auth, _ := ctx.Value(authInfoKey{}).(kagentapi.AuthInfo)
	return auth
}

// ForwardingExecutor implements a2asrv.AgentExecutor. It resolves the target
// Klaus pod via the routing table, forwards the inbound message over A2A
// streaming, re-emits the pod's events under the gateway's own task context,
// and pushes the completed turn to kagent.
type ForwardingExecutor struct {
	Router  Resolver
	Dial    Dialer
	Kagent  KagentPusher
	cancels sync.Map // a2apkg.TaskID → context.CancelFunc
}

// Execute resolves the pod, streams the A2A call, and pushes to kagent.
func (e *ForwardingExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	auth := AuthInfoFromContext(ctx)

	if err := queue.Write(ctx, workingEvent(reqCtx, "")); err != nil {
		return fmt.Errorf("write working event: %w", err)
	}

	ref, err := e.Router.Resolve(ctx, routing.InboundMessage{
		Channel:   "a2a",
		ChannelID: reqCtx.ContextID,
		UserID:    auth.UserSub,
	})
	if err != nil {
		slog.WarnContext(ctx, "a2a: route not found", "contextID", reqCtx.ContextID, "error", err)
		if qerr := queue.Write(ctx, failedEvent(reqCtx, "no route for context: "+reqCtx.ContextID)); qerr != nil {
			return fmt.Errorf("write failed event: %w", qerr)
		}
		return nil
	}

	podClient, err := e.Dial.For(ctx, ref.BaseURL)
	if err != nil {
		slog.ErrorContext(ctx, "a2a: dial pod", "baseURL", ref.BaseURL, "error", err)
		if qerr := queue.Write(ctx, failedEvent(reqCtx, "internal error: dial pod")); qerr != nil {
			return fmt.Errorf("write failed event: %w", qerr)
		}
		return nil
	}

	// podCtx is canceled when Cancel() is called for this task, interrupting the
	// pod stream before it finishes naturally.
	podCtx, cancelPod := context.WithCancel(ctx)
	e.cancels.Store(reqCtx.TaskID, cancelPod)
	defer func() {
		cancelPod()
		e.cancels.Delete(reqCtx.TaskID)
	}()

	// Forward with the original contextID so the pod can resume the session.
	// Message.Metadata and params.Metadata carry per-request hints (effort,
	// max_budget_usd, json_schema) that the pod executor reads from; pass them
	// through unchanged.
	params := &a2apkg.MessageSendParams{
		Metadata: reqCtx.Metadata,
		Message: &a2apkg.Message{
			ID:        reqCtx.Message.ID,
			Role:      reqCtx.Message.Role,
			Parts:     reqCtx.Message.Parts,
			ContextID: reqCtx.ContextID,
			Metadata:  reqCtx.Message.Metadata,
		},
	}

	var (
		agentText        strings.Builder
		artifactID       a2apkg.ArtifactID
		firstArtifact    = true
		finalStatusEvent *a2apkg.TaskStatusUpdateEvent
	)
	userText := extractText(reqCtx.Message)

loop:
	for event, err := range podClient.SendStreamingMessage(podCtx, params) {
		if err != nil {
			if podCtx.Err() != nil {
				// Canceled via Cancel(); the cancel event is already queued.
				return nil
			}
			slog.WarnContext(ctx, "a2a: stream error from pod", "error", err)
			if qerr := queue.Write(ctx, failedEvent(reqCtx, "upstream error: "+err.Error())); qerr != nil {
				return fmt.Errorf("write failed event: %w", qerr)
			}
			return nil
		}

		switch ev := event.(type) {
		case *a2apkg.TaskStatusUpdateEvent:
			if ev.Status.State == a2apkg.TaskStateWorking || ev.Status.State == a2apkg.TaskStateSubmitted {
				if err := queue.Write(ctx, workingEvent(reqCtx, extractText(ev.Status.Message))); err != nil {
					return fmt.Errorf("write working event: %w", err)
				}
			}
			if ev.Final {
				finalStatusEvent = ev
				break loop
			}

		case *a2apkg.TaskArtifactUpdateEvent:
			if ev.Artifact == nil {
				continue
			}
			text := extractTextFromParts(ev.Artifact.Parts)
			agentText.WriteString(text)
			var gwEvent *a2apkg.TaskArtifactUpdateEvent
			if firstArtifact {
				gwEvent = artifactEvent(reqCtx, text)
				artifactID = gwEvent.Artifact.ID
				firstArtifact = false
			} else {
				gwEvent = artifactUpdateEvent(reqCtx, artifactID, text)
			}
			if err := queue.Write(ctx, gwEvent); err != nil {
				return fmt.Errorf("write artifact event: %w", err)
			}
		}
	}

	// Mirror the pod's terminal state; default to completed when no final event arrived.
	var finalWriteErr error
	switch {
	case finalStatusEvent == nil:
		finalWriteErr = queue.Write(ctx, completedEvent(reqCtx))
	case finalStatusEvent.Status.State == a2apkg.TaskStateFailed:
		errText := extractText(finalStatusEvent.Status.Message)
		if errText == "" {
			errText = "upstream task failed"
		}
		finalWriteErr = queue.Write(ctx, failedEvent(reqCtx, errText))
	case finalStatusEvent.Status.State == a2apkg.TaskStateCanceled:
		finalWriteErr = queue.Write(ctx, canceledEvent(reqCtx))
	case finalStatusEvent.Status.State == a2apkg.TaskStateInputRequired:
		finalWriteErr = queue.Write(ctx, inputRequiredEvent(reqCtx, extractText(finalStatusEvent.Status.Message)))
	case finalStatusEvent.Status.State == a2apkg.TaskStateAuthRequired:
		finalWriteErr = queue.Write(ctx, authRequiredEvent(reqCtx, extractText(finalStatusEvent.Status.Message)))
	case finalStatusEvent.Status.State == a2apkg.TaskStateRejected:
		finalWriteErr = queue.Write(ctx, rejectedEvent(reqCtx, extractText(finalStatusEvent.Status.Message)))
	default:
		finalWriteErr = queue.Write(ctx, completedEvent(reqCtx))
	}
	if finalWriteErr != nil {
		return fmt.Errorf("write final event: %w", finalWriteErr)
	}

	// Push to kagent best-effort, asynchronously so the A2A response is not delayed.
	if e.Kagent.Enabled() {
		go e.pushToKagent(context.WithoutCancel(ctx), reqCtx, userText, agentText.String(), auth)
	}

	return nil
}

// Cancel interrupts the in-flight pod stream (if any) and writes a canceled event.
func (e *ForwardingExecutor) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	if fn, ok := e.cancels.LoadAndDelete(reqCtx.TaskID); ok {
		fn.(context.CancelFunc)()
	}
	return queue.Write(ctx, canceledEvent(reqCtx))
}

func (e *ForwardingExecutor) pushToKagent(ctx context.Context, reqCtx *a2asrv.RequestContext, userText, agentText string, auth kagentapi.AuthInfo) {
	sessionID := auth.UserSub + ":" + reqCtx.ContextID
	taskID := string(reqCtx.TaskID)

	userEvent := kagentapi.NewSessionEvent(taskID+"-user", "user", userText)
	e.Kagent.PushEvent(ctx, sessionID, userEvent, auth)

	agentEvent := kagentapi.NewSessionEvent(taskID+"-agent", "agent", agentText)
	e.Kagent.PushEvent(ctx, sessionID, agentEvent, auth)

	e.Kagent.StoreTask(ctx, taskID, reqCtx.ContextID, userText, agentText, auth)
}
