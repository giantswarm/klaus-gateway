package a2a

import (
	"context"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	a2aclient "github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

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
	if _, err := p.client.CancelTask(cancelCtx, &a2apkg.CancelTaskRequest{ID: id}); err != nil {
		slog.WarnContext(ctx, "a2a: cancel task at pod", "podTaskID", id, "error", err)
	}
}

var _ a2asrv.AgentExecutor = (*ForwardingExecutor)(nil)

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
func (e *ForwardingExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		auth := AuthInfoFromContext(ctx)

		if execCtx.StoredTask == nil {
			if !yield(a2apkg.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}

		if !yield(workingEvent(execCtx, ""), nil) {
			return
		}

		ref, err := e.Router.Resolve(ctx, routing.InboundMessage{
			Channel:   "a2a",
			ChannelID: execCtx.ContextID,
			UserID:    auth.UserSub,
		})
		if err != nil {
			slog.WarnContext(ctx, "a2a: route not found", "contextID", execCtx.ContextID, "error", err)
			yield(failedEvent(execCtx, "no route for context: "+execCtx.ContextID), nil)
			return
		}

		podClient, err := e.Dial.For(ctx, ref.BaseURL)
		if err != nil {
			slog.ErrorContext(ctx, "a2a: dial pod", "baseURL", ref.BaseURL, "error", err)
			yield(failedEvent(execCtx, "internal error: dial pod"), nil)
			return
		}

		// podCtx is canceled when Cancel() is called or when a newer Execute() for
		// the same contextID preempts this one.
		podCtx, cancelPod := context.WithCancel(ctx)
		exec := newPodExec(cancelPod, podClient)

		e.cancels.Store(execCtx.TaskID, exec)

		// Preempt any previous forward for this contextID. The Klaus pod holds a
		// per-contextID in-flight lock; the previous pod execution must be released
		// before this one can proceed.
		if old, loaded := e.contextCancels.Swap(execCtx.ContextID, exec); loaded && old != nil {
			prev := old.(*podExec)
			prev.cancelPod()
			go prev.cancelAtPod(ctx) // best-effort; async to not block this forward
		}

		defer func() {
			cancelPod()
			e.cancels.Delete(execCtx.TaskID)
			// Only clear contextCancels if our entry is still there (a newer Execute()
			// may have replaced it already).
			e.contextCancels.CompareAndDelete(execCtx.ContextID, exec)
		}()

		// Push the user message to kagent before starting the pod stream so the
		// session history is populated while Klaus is thinking.
		userText := extractText(execCtx.Message)
		if e.Kagent.Enabled() {
			taskID := string(execCtx.TaskID)
			e.Kagent.PushEvent(ctx, execCtx.ContextID, kagentapi.NewSessionEvent(taskID+"-user", "user", userText), auth)
		}

		// Forward with the original contextID so the pod can resume the session.
		// Message.Metadata and params.Metadata carry per-request hints (effort,
		// max_budget_usd, json_schema) that the pod executor reads from; pass them
		// through unchanged.
		params := &a2apkg.SendMessageRequest{
			Metadata: execCtx.Metadata,
			Message: &a2apkg.Message{
				ID:        execCtx.Message.ID,
				Role:      execCtx.Message.Role,
				Parts:     execCtx.Message.Parts,
				ContextID: execCtx.ContextID,
				Metadata:  execCtx.Message.Metadata,
			},
		}

		var (
			agentText             strings.Builder
			artifactID            a2apkg.ArtifactID
			firstArtifact         = true
			artifactLastChunkSent bool
			finalStatusEvent      *a2apkg.TaskStatusUpdateEvent
		)

	loop:
		for event, err := range podClient.SendStreamingMessage(podCtx, params) {
			if err != nil {
				if podCtx.Err() != nil {
					// Canceled via Cancel() or preempted by a newer request; the
					// caller has already written or will write the terminal event.
					return
				}
				slog.WarnContext(ctx, "a2a: stream error from pod", "error", err)
				yield(failedEvent(execCtx, "upstream error: "+err.Error()), nil)
				return
			}

			// Capture the pod's task ID from the first event so Cancel() can issue
			// tasks/cancel directly to the pod, releasing its per-context lock.
			exec.setTaskID(event.TaskInfo().TaskID)

			switch ev := event.(type) {
			case *a2apkg.TaskStatusUpdateEvent:
				if ev.Status.State == a2apkg.TaskStateWorking || ev.Status.State == a2apkg.TaskStateSubmitted {
					if !yield(workingEvent(execCtx, extractText(ev.Status.Message)), nil) {
						return
					}
				}
				if ev.Status.State.Terminal() {
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
					gwEvent = artifactEvent(execCtx, text)
					artifactID = gwEvent.Artifact.ID
					firstArtifact = false
				} else {
					gwEvent = artifactUpdateEvent(execCtx, artifactID, text)
				}
				gwEvent.LastChunk = ev.LastChunk
				if ev.LastChunk {
					artifactLastChunkSent = true
				}
				if !yield(gwEvent, nil) {
					return
				}
			}
		}

		// The kagent UI only commits accumulated artifact text to React state when it
		// receives an artifact event with LastChunk=true. If the pod never sent one
		// (common when the pod streams a single consolidated chunk), emit an empty
		// flush here so the UI finalises the message before the terminal event arrives.
		if !firstArtifact && !artifactLastChunkSent {
			flush := artifactUpdateEvent(execCtx, artifactID, "")
			flush.LastChunk = true
			if !yield(flush, nil) {
				return
			}
		}

		// Push agent response and task record to kagent before flushing the terminal
		// event. The UI polls session events after the A2A stream closes; pushing here
		// (not async) ensures the data is present when that poll lands.
		if e.Kagent.Enabled() {
			taskID := string(execCtx.TaskID)
			finalState := string(a2apkg.TaskStateCompleted)
			if finalStatusEvent != nil {
				finalState = string(finalStatusEvent.Status.State)
			}
			agentEventCtx := context.WithoutCancel(ctx)
			e.Kagent.PushEvent(agentEventCtx, execCtx.ContextID, kagentapi.NewSessionEvent(taskID+"-agent", "agent", agentText.String()), auth)
			e.Kagent.StoreTask(agentEventCtx, taskID, execCtx.ContextID, userText, agentText.String(), finalState, auth)
		}

		// Mirror the pod's terminal state; default to completed when no final event arrived.
		switch {
		case finalStatusEvent == nil:
			yield(completedEvent(execCtx), nil)
		case finalStatusEvent.Status.State == a2apkg.TaskStateCompleted:
			yield(completedEvent(execCtx), nil)
		case finalStatusEvent.Status.State == a2apkg.TaskStateFailed:
			errText := extractText(finalStatusEvent.Status.Message)
			if errText == "" {
				errText = "upstream task failed"
			}
			yield(failedEvent(execCtx, errText), nil)
		case finalStatusEvent.Status.State == a2apkg.TaskStateCanceled:
			yield(canceledEvent(execCtx), nil)
		case finalStatusEvent.Status.State == a2apkg.TaskStateInputRequired:
			yield(inputRequiredEvent(execCtx, extractText(finalStatusEvent.Status.Message)), nil)
		case finalStatusEvent.Status.State == a2apkg.TaskStateAuthRequired:
			yield(authRequiredEvent(execCtx, extractText(finalStatusEvent.Status.Message)), nil)
		case finalStatusEvent.Status.State == a2apkg.TaskStateRejected:
			yield(rejectedEvent(execCtx, extractText(finalStatusEvent.Status.Message)), nil)
		default:
			slog.WarnContext(ctx, "a2a: unexpected terminal state from pod", "state", finalStatusEvent.Status.State)
			yield(failedEvent(execCtx, "unexpected terminal state: "+string(finalStatusEvent.Status.State)), nil)
		}
	}
}

// Cancel interrupts the in-flight pod stream (if any), propagates the cancel to
// the Klaus pod itself, and yields a canceled event.
func (e *ForwardingExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		raw, ok := e.cancels.LoadAndDelete(execCtx.TaskID)
		if !ok {
			return
		}
		exec := raw.(*podExec)
		exec.cancelPod()
		go exec.cancelAtPod(ctx) // propagate to pod; async to not delay the canceled event
		yield(canceledEvent(execCtx), nil)
	}
}

