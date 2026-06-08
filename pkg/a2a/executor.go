package a2a

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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
	StoreTask(ctx context.Context, taskID, contextID, userText, agentText, state string, auth kagentapi.AuthInfo)
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

// podExec tracks an in-flight forward to a Klaus pod. The zero value is not
// useful; always create via newPodExec.
type podExec struct {
	cancelPod context.CancelFunc
	client    *a2aclient.Client

	// mu guards taskID. taskID is set once, on the first streamed event from
	// the pod; zero value means the stream has not yet produced an event.
	mu     sync.Mutex
	taskID a2apkg.TaskID
}

func newPodExec(cancel context.CancelFunc, client *a2aclient.Client) *podExec {
	return &podExec{cancelPod: cancel, client: client}
}

// setTaskID records the pod's task ID on the first event; subsequent calls are
// no-ops.
func (p *podExec) setTaskID(id a2apkg.TaskID) {
	p.mu.Lock()
	if p.taskID == "" {
		p.taskID = id
	}
	p.mu.Unlock()
}

// cancelAtPod sends tasks/cancel to the Klaus pod using the task ID captured
// from the pod's first streamed event. Best-effort: if the task ID is not yet
// known the call is skipped. Errors are logged only.
func (p *podExec) cancelAtPod(ctx context.Context) {
	p.mu.Lock()
	id := p.taskID
	p.mu.Unlock()
	if id == "" || p.client == nil {
		return
	}
	cancelCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	if _, err := p.client.CancelTask(cancelCtx, &a2apkg.TaskIDParams{ID: id}); err != nil {
		slog.WarnContext(ctx, "a2a: cancel task at pod", "podTaskID", id, "error", err)
	}
}

// ForwardingExecutor implements a2asrv.AgentExecutor. It resolves the target
// Klaus pod via the routing table, forwards the inbound message over A2A
// streaming, re-emits the pod's events under the gateway's own task context,
// and pushes the completed turn to kagent.
type ForwardingExecutor struct {
	Router Resolver
	Dial   Dialer
	Kagent KagentPusher

	// cancels maps gateway TaskID → *podExec for the a2asrv-driven Cancel() path.
	cancels sync.Map // a2apkg.TaskID → *podExec
	// contextCancels maps contextID → *podExec so that a new Execute() for the
	// same context can preempt the previous forward before starting its own.
	contextCancels sync.Map // string(contextID) → *podExec
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

	// podCtx is canceled when Cancel() is called or when a newer Execute() for
	// the same contextID preempts this one.
	podCtx, cancelPod := context.WithCancel(ctx)
	exec := newPodExec(cancelPod, podClient)

	e.cancels.Store(reqCtx.TaskID, exec)

	// Preempt any previous forward for this contextID. The Klaus pod holds a
	// per-contextID in-flight lock; the previous pod execution must be released
	// before this one can proceed.
	if old, loaded := e.contextCancels.Swap(reqCtx.ContextID, exec); loaded && old != nil {
		prev := old.(*podExec)
		prev.cancelPod()
		go prev.cancelAtPod(ctx) // best-effort; async to not block this forward
	}

	defer func() {
		cancelPod()
		e.cancels.Delete(reqCtx.TaskID)
		// Only clear contextCancels if our entry is still there (a newer Execute()
		// may have replaced it already).
		e.contextCancels.CompareAndDelete(reqCtx.ContextID, exec)
	}()

	// Push the user message to kagent before starting the pod stream so the
	// session history is populated while Klaus is thinking.
	userText := extractText(reqCtx.Message)
	if e.Kagent.Enabled() {
		taskID := string(reqCtx.TaskID)
		e.Kagent.PushEvent(ctx, reqCtx.ContextID, kagentapi.NewSessionEvent(taskID+"-user", "user", userText), auth)
	}

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

loop:
	for event, err := range podClient.SendStreamingMessage(podCtx, params) {
		if err != nil {
			if podCtx.Err() != nil {
				// Canceled via Cancel() or preempted by a newer request; the
				// caller has already written or will write the terminal event.
				return nil
			}
			slog.WarnContext(ctx, "a2a: stream error from pod", "error", err)
			if qerr := queue.Write(ctx, failedEvent(reqCtx, "upstream error: "+err.Error())); qerr != nil {
				return fmt.Errorf("write failed event: %w", qerr)
			}
			return nil
		}

		// Capture the pod's task ID from the first event so Cancel() can issue
		// tasks/cancel directly to the pod, releasing its per-context lock.
		exec.setTaskID(event.TaskInfo().TaskID)

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

	// Push agent response and task record to kagent before flushing the terminal
	// event. The UI polls session events after the A2A stream closes; pushing here
	// (not async) ensures the data is present when that poll lands.
	if e.Kagent.Enabled() {
		taskID := string(reqCtx.TaskID)
		finalState := string(a2apkg.TaskStateCompleted)
		if finalStatusEvent != nil {
			finalState = string(finalStatusEvent.Status.State)
		}
		agentEventCtx := context.WithoutCancel(ctx)
		e.Kagent.PushEvent(agentEventCtx, reqCtx.ContextID, kagentapi.NewSessionEvent(taskID+"-agent", "agent", agentText.String()), auth)
		e.Kagent.StoreTask(agentEventCtx, taskID, reqCtx.ContextID, userText, agentText.String(), finalState, auth)
	}

	// Mirror the pod's terminal state; default to completed when no final event arrived.
	var finalWriteErr error
	switch {
	case finalStatusEvent == nil:
		finalWriteErr = queue.Write(ctx, completedEvent(reqCtx))
	case finalStatusEvent.Status.State == a2apkg.TaskStateCompleted:
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
		slog.WarnContext(ctx, "a2a: unexpected terminal state from pod", "state", finalStatusEvent.Status.State)
		finalWriteErr = queue.Write(ctx, failedEvent(reqCtx, "unexpected terminal state: "+string(finalStatusEvent.Status.State)))
	}
	if finalWriteErr != nil {
		return fmt.Errorf("write final event: %w", finalWriteErr)
	}

	return nil
}

// Cancel interrupts the in-flight pod stream (if any), propagates the cancel to
// the Klaus pod itself, and writes a canceled event.
func (e *ForwardingExecutor) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	raw, ok := e.cancels.LoadAndDelete(reqCtx.TaskID)
	if !ok {
		return nil
	}
	exec := raw.(*podExec)
	exec.cancelPod()
	go exec.cancelAtPod(ctx) // propagate to pod; async to not delay the canceled event
	return queue.Write(ctx, canceledEvent(reqCtx))
}

