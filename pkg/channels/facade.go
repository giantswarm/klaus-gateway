package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/instance"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// InstanceClient is the slice of pkg/instance.Client that the Facade needs.
// Tests can inject a fake without standing up an HTTP server.
type InstanceClient interface {
	StreamCompletion(ctx context.Context, ref InstanceRef, body []byte) (io.ReadCloser, error)
	Messages(ctx context.Context, ref InstanceRef, threadID string) (instance.MessagesResponse, error)
}

// AgentClient is the slice of pkg/a2a.Client the Facade needs to run a
// conversation on a kagent API v2 controller: the A2A v1 calls on an
// AgentInstance, the instance lifecycle, and the AgentTemplate roster. When
// nil, SendCompletion falls back to the OpenAI /v1 path unconditionally.
type AgentClient interface {
	// Stream sends msg to the instance and yields the task's events.
	Stream(ctx context.Context, instanceID string, msg *a2apkg.Message) iter.Seq2[a2apkg.Event, error]
	// GetTask returns one task of the instance.
	GetTask(ctx context.Context, instanceID string, taskID a2apkg.TaskID) (*a2apkg.Task, error)
	// CancelTask cancels a running task server-side.
	CancelTask(ctx context.Context, instanceID string, taskID a2apkg.TaskID) (*a2apkg.Task, error)
	// CreateInstance creates (idempotently per requestID) the instance for a
	// conversation with agentRef and returns it once it is ready.
	CreateInstance(ctx context.Context, agentRef, requestID string) (pkga2a.Instance, error)
	// GetInstance returns an instance; pkga2a.ErrInstanceNotFound when gone.
	GetInstance(ctx context.Context, id string) (pkga2a.Instance, error)
	// DeleteInstance removes an instance; a missing one is not an error.
	DeleteInstance(ctx context.Context, id string) error
	// ListAgents lists the selectable agents of the served namespace.
	ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error)
}

// Facade wires the routing.Router, instance.Client, and lifecycle.Manager
// together into the Gateway surface used by channel adapters.
type Facade struct {
	Router    *routing.Router
	Client    InstanceClient
	Lifecycle lifecycle.Manager
	// Agent, when non-nil, routes channel turns that carry an AgentRef to a
	// kagent controller instead of the OpenAI /v1 path.
	Agent AgentClient
	// Routes persists the thread -> AgentInstance binding across restarts.
	// Required when Agent is set.
	Routes store.Store
}

// ListAgents lists the agents a channel may select. Unavailable when no
// kagent client is configured.
func (f *Facade) ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error) {
	if f == nil || f.Agent == nil {
		return nil, errors.New("channels: no agent client configured")
	}
	return f.Agent.ListAgents(ctx)
}

// SessionResumable reports whether msg's thread is bound to an AgentInstance
// the controller still knows, so a reply resumes it rather than starting
// fresh. checked is false when no kagent client is configured or the lookup
// errored; exists is then meaningless and the caller should stay silent. A
// binding whose instance the controller no longer has is dropped, so the next
// turn creates a fresh one. The lookup is bounded by a short timeout: it sits
// before the turn, so a slow controller must not stall the first reply.
func (f *Facade) SessionResumable(ctx context.Context, msg InboundMessage) (exists, checked bool) {
	if f == nil || f.Agent == nil || f.Routes == nil {
		return false, false
	}
	ctx = withChannelAuth(ctx, msg)
	ctx, cancel := context.WithTimeout(ctx, sessionCheckTimeout)
	defer cancel()
	key := instanceKey(msg)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		return false, false
	}
	if !ok || entry.AgentInstanceID == "" {
		return false, true
	}
	if _, err := f.Agent.GetInstance(ctx, entry.AgentInstanceID); err != nil {
		if !pkga2a.IsNotFound(err) {
			return false, false
		}
		_ = f.Routes.Delete(ctx, key)
		return false, true
	}
	return true, true
}

// sessionCheckTimeout bounds the resume existence-check, which runs on the
// turn's critical path.
const sessionCheckTimeout = 3 * time.Second

// ResetSession deletes the AgentInstance bound to msg's thread and drops the
// binding, so the next turn starts a fresh instance. Used when the
// conversation's history has become unusable (the model API rejects it on
// every turn). Returns false when no kagent client is configured or the
// thread has no binding.
func (f *Facade) ResetSession(ctx context.Context, msg InboundMessage) (bool, error) {
	if f == nil || f.Agent == nil || f.Routes == nil {
		return false, nil
	}
	ctx = withChannelAuth(ctx, msg)
	ctx, cancel := context.WithTimeout(ctx, sessionCheckTimeout)
	defer cancel()
	key := instanceKey(msg)
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		return false, err
	}
	if !ok || entry.AgentInstanceID == "" {
		return false, nil
	}
	if err := f.Agent.DeleteInstance(ctx, entry.AgentInstanceID); err != nil {
		return false, err
	}
	if err := f.Routes.Delete(ctx, key); err != nil {
		return false, err
	}
	return true, nil
}

// Resolve maps an InboundMessage to a live InstanceRef via the routing
// table (creating a new instance on miss when the router has auto-create
// enabled). On the kagent path (Agent set and AgentRef non-empty) routing is
// bypassed and a zero InstanceRef is returned — the thread's AgentInstance is
// resolved when the turn is sent.
func (f *Facade) Resolve(ctx context.Context, in InboundMessage) (InstanceRef, error) {
	if f == nil || f.Router == nil {
		return InstanceRef{}, errors.New("channels: facade router is nil")
	}
	if f.Agent != nil && in.AgentRef != "" {
		return InstanceRef{}, nil
	}
	ref, err := f.Router.Resolve(ctx, routing.InboundMessage{
		Channel:   in.Channel,
		ChannelID: in.ChannelID,
		UserID:    in.UserID,
		ThreadID:  in.ThreadID,
	})
	if err != nil {
		return InstanceRef{}, err
	}
	return ref, nil
}

// SendCompletion streams a completion for msg. When Agent is set and
// msg.AgentRef is non-empty, the turn runs on the thread's AgentInstance;
// otherwise it falls back to the OpenAI /v1 SSE path.
//
// The caller must receive from the returned channel until it closes.
func (f *Facade) SendCompletion(ctx context.Context, ref InstanceRef, msg InboundMessage) (<-chan OutboundDelta, error) {
	if f.Agent != nil && msg.AgentRef != "" {
		return f.sendViaA2A(ctx, msg)
	}
	return f.sendViaOpenAI(ctx, ref, msg)
}

// instanceKey is the routing-store key of a thread's AgentInstance binding.
// The user slot is empty on purpose: a thread is shared by its participants,
// so every one of them reaches the same instance.
func instanceKey(msg InboundMessage) store.Key {
	return store.Key{Channel: msg.Channel, ChannelID: msg.ChannelID, ThreadID: msg.ThreadID, Agent: msg.AgentRef}
}

// instanceFor returns the AgentInstance id msg's thread is bound to, creating
// the instance on the thread's first turn. The create is keyed by the
// synthesized context id, so a retried first turn does not create a second
// instance. The binding never expires on its own: the instance is the
// conversation, and the controller keeps it until it is deleted.
func (f *Facade) instanceFor(ctx context.Context, msg InboundMessage) (string, error) {
	if f.Routes == nil {
		return "", errors.New("channels: no routing store for the agent instance binding")
	}
	key := instanceKey(msg)
	now := time.Now()
	entry, ok, err := f.Routes.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("channels: read instance binding: %w", err)
	}
	if ok && entry.AgentInstanceID != "" {
		entry.LastSeen = now
		if err := f.Routes.Put(ctx, key, entry); err != nil {
			return "", fmt.Errorf("channels: refresh instance binding: %w", err)
		}
		return entry.AgentInstanceID, nil
	}
	requestID := SynthesizeContextID(msg.Channel, msg.ChannelID, "", msg.ThreadID, msg.AgentRef)
	inst, err := f.Agent.CreateInstance(ctx, msg.AgentRef, requestID)
	if err != nil {
		return "", err
	}
	if err := f.Routes.Put(ctx, key, store.Entry{AgentInstanceID: inst.ID, CreatedAt: now, LastSeen: now}); err != nil {
		return "", fmt.Errorf("channels: store instance binding: %w", err)
	}
	return inst.ID, nil
}

// cancelTimeout bounds the server-side cancel of a task whose channel turn
// was stopped.
const cancelTimeout = 10 * time.Second

// sendViaA2A runs the turn on the thread's AgentInstance and maps the task's
// events to OutboundDeltas. The controller's refusal of a turn (an unknown or
// unavailable agent, an instance still working on a previous message, a
// resume that names no paused prompt) is returned synchronously so channels
// can render it; the stream's own failures arrive as error deltas.
func (f *Facade) sendViaA2A(ctx context.Context, msg InboundMessage) (<-chan OutboundDelta, error) {
	ctx = withChannelAuth(ctx, msg)

	instanceID, err := f.instanceFor(ctx, msg)
	if err != nil {
		return nil, err
	}
	message, err := f.outboundMessage(ctx, instanceID, msg)
	if err != nil {
		return nil, err
	}

	next, stop := iter.Pull2(f.Agent.Stream(ctx, instanceID, message))
	first, err, ok := next()
	if err != nil {
		stop()
		return nil, err
	}
	if !ok {
		stop()
		return nil, errors.New("a2a: stream ended without events")
	}

	out := make(chan OutboundDelta, 16)
	go func() {
		defer close(out)
		defer stop()
		var taskID a2apkg.TaskID
		terminated := false
		event, streamErr := first, error(nil)
		for {
			if streamErr != nil {
				terminated = f.emit(ctx, out, OutboundDelta{Err: streamErr})
				break
			}
			if info, ok := event.(a2apkg.TaskInfoProvider); ok && info.TaskInfo().TaskID != "" {
				taskID = info.TaskInfo().TaskID
			}
			for _, delta := range mapA2AEvent(event) {
				if delta.isZero() {
					continue
				}
				if !f.emit(ctx, out, delta) {
					terminated = true
					break
				}
				if delta.Err != nil || delta.Done || delta.Kind == DeltaPrompt {
					terminated = true
					break
				}
			}
			if terminated {
				break
			}
			var more bool
			event, streamErr, more = next()
			if !more {
				break
			}
		}
		if ctx.Err() != nil && taskID != "" {
			f.cancelTask(ctx, instanceID, taskID)
			return
		}
		if !terminated {
			f.emit(ctx, out, OutboundDelta{Err: errors.New("a2a: stream ended without terminal status")})
		}
	}()
	return out, nil
}

// emit delivers delta unless ctx is done; it reports whether the delta was
// delivered.
func (f *Facade) emit(ctx context.Context, out chan<- OutboundDelta, delta OutboundDelta) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- delta:
		return true
	}
}

// cancelTask cancels the task server-side after the channel stopped the turn,
// so the agent stops working instead of running on unobserved. The cancel
// keeps the turn's identity but not its cancellation.
func (f *Facade) cancelTask(ctx context.Context, instanceID string, taskID a2apkg.TaskID) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	if _, err := f.Agent.CancelTask(cctx, instanceID, taskID); err != nil {
		slog.Warn("a2a: cancel task after stopped turn failed", "instance", instanceID, "task", taskID, "error", err)
	}
}

// outboundMessage builds the A2A message of a turn. A fresh turn carries no
// task id (the controller assigns one) and no context id (the controller owns
// it). A HITL decision resumes the paused task: the message carries that task's
// id, and the typed response — built against the request the task is paused
// on — rides as the HITL extension payload.
func (f *Facade) outboundMessage(ctx context.Context, instanceID string, msg InboundMessage) (*a2apkg.Message, error) {
	message := a2apkg.NewMessage(a2apkg.MessageRoleUser, buildInboundParts(msg)...)
	if msg.TaskID == "" {
		return message, nil
	}
	message.TaskID = a2apkg.TaskID(msg.TaskID)
	if msg.Decision == nil {
		return message, nil
	}
	task, err := f.Agent.GetTask(ctx, instanceID, message.TaskID)
	if err != nil {
		return nil, fmt.Errorf("a2a: load the paused task %s: %w", msg.TaskID, err)
	}
	if task.Status.State != a2apkg.TaskStateInputRequired {
		return nil, fmt.Errorf("%w: task %s is %s", ErrNoPendingPrompt, msg.TaskID, task.Status.State)
	}
	request, err := pkga2a.ParseHITLRequest(task.Status.Message)
	if err != nil {
		return nil, fmt.Errorf("a2a: read the paused task's prompt: %w", err)
	}
	if request == nil {
		return nil, fmt.Errorf("%w: task %s carries no HITL request", ErrNoPendingPrompt, msg.TaskID)
	}
	if err := pkga2a.AttachHITL(message, hitlResponse(request, msg.Decision)); err != nil {
		return nil, err
	}
	return message, nil
}

// ErrNoPendingPrompt is returned when a HITL decision arrives for a task that
// is not paused on a prompt any more.
var ErrNoPendingPrompt = errors.New("a2a: the task is not waiting for a decision")

// withChannelAuth seeds ctx with the target agent ref, the originating channel
// name, and the caller's forwarded bearer token for the A2A client. The channel
// name lets the token source enforce per-channel credential policy (e.g. no
// service-account fallback for Slack when account linking is enabled).
func withChannelAuth(ctx context.Context, msg InboundMessage) context.Context {
	ctx = pkga2a.WithAgentRef(ctx, msg.AgentRef)
	ctx = pkga2a.WithChannel(ctx, msg.Channel)
	ctx = pkga2a.WithForwardedToken(ctx, msg.BearerToken)
	return ctx
}

// mapA2AEvent converts a single A2A streaming event to zero or more
// OutboundDeltas. A single event may carry both assistant text and tool-call
// DataParts, so it can expand to several deltas. Non-completed terminal states
// (failed, rejected, canceled) map to an error delta so channels surface them
// rather than silently closing.
//
// kagent attaches token usage to the event/message metadata (not to a part),
// as per-LLM-call deltas on interim working events; the terminal completed
// event carries no usage, so consumers sum the interim deltas. Partial
// (streaming) events mirror their call's usage and are skipped to avoid
// counting one call several times. Tool activity rides on
// function_call/function_response DataParts. A paused task's prompt rides on
// the input-required status message as the HITL extension payload.
func mapA2AEvent(event a2apkg.Event) []OutboundDelta {
	switch ev := event.(type) {
	case *a2apkg.TaskArtifactUpdateEvent:
		if ev.Artifact == nil {
			return nil
		}
		return append(textDelta(ev.Artifact.Parts), toolActivityDeltas(ev.Artifact.Parts)...)
	case *a2apkg.TaskStatusUpdateEvent:
		var usage *TurnUsage
		if !isPartialStatusUpdate(ev) {
			usage = parseTurnUsage(ev.Metadata)
			if usage == nil && ev.Status.Message != nil {
				usage = parseTurnUsage(ev.Status.Message.Metadata)
			}
		}
		return mapTaskStatus(ev.TaskID, ev.Status, usage, func() []OutboundDelta {
			// Interim working event: surface the narration the agent wrote for the
			// tool calls the same message carries, the tool activity itself, and a
			// usage-only delta when the event reports usage.
			parts := messageParts(ev.Status.Message)
			deltas := narrationDeltas(ev, parts)
			deltas = append(deltas, toolActivityDeltas(parts)...)
			if usage != nil {
				deltas = append(deltas, OutboundDelta{Usage: usage})
			}
			return deltas
		})
	case *a2apkg.Task:
		// A whole task arrives as the first event of a stream (the submitted
		// task the controller stored) and as the reply to a message the
		// controller answered from its store. Only a quiescent state carries
		// something to render; the submitted snapshot is the turn starting.
		if ev.Status.State == a2apkg.TaskStateSubmitted || ev.Status.State == a2apkg.TaskStateWorking {
			return nil
		}
		return mapTaskStatus(ev.ID, ev.Status, parseTurnUsage(ev.Metadata), func() []OutboundDelta { return nil })
	case *a2apkg.Message:
		// A bare agent message is a complete reply without a task wrapper.
		return append(textDelta(ev.Parts), OutboundDelta{Done: true})
	}
	return nil
}

// mapTaskStatus maps a task state to deltas: completed closes the turn,
// input-required/auth-required pauses it on a prompt, any other terminal state
// fails it, and a working state is left to interim.
func mapTaskStatus(taskID a2apkg.TaskID, status a2apkg.TaskStatus, usage *TurnUsage, interim func() []OutboundDelta) []OutboundDelta {
	switch status.State {
	case a2apkg.TaskStateCompleted:
		return []OutboundDelta{{Done: true, Usage: usage}}
	case a2apkg.TaskStateInputRequired, a2apkg.TaskStateAuthRequired:
		var hitl *HitlPrompt
		text := ""
		if status.Message != nil {
			hitl = parseHitlPrompt(status.Message)
			text = extractTextFromA2AParts(status.Message.Parts)
		}
		// The status message's text is the agent's hint; when it carries none,
		// fall back to the structured prompt's summary for plain-text renderers.
		if text == "" && hitl != nil {
			text = hitl.summary()
		}
		return []OutboundDelta{{Kind: DeltaPrompt, Content: text, TaskID: string(taskID), Prompt: hitl, Usage: usage}}
	default:
		if status.State.Terminal() {
			msg := fmt.Sprintf("a2a: task ended with state %s", status.State)
			if status.Message != nil {
				if text := extractTextFromA2AParts(status.Message.Parts); text != "" {
					msg = text
				}
			}
			return []OutboundDelta{{Err: errors.New(msg), Usage: usage}}
		}
		return interim()
	}
}

// textDelta returns a single-element slice with the concatenated text of parts,
// or nil when there is no text.
func textDelta(parts a2apkg.ContentParts) []OutboundDelta {
	if text := extractTextFromA2AParts(parts); text != "" {
		return []OutboundDelta{{Content: text}}
	}
	return nil
}

// narrationDeltas returns the agent's interim narration for a working status
// message: the text it wrote just before the tool calls the same message
// carries. Requiring a function_call part is what separates narration from
// kagent's other text-bearing working events — the text-only mirror of the final
// answer (rendered from the artifact, so this would duplicate it), the echo of
// the user's own message, and partial streaming chunks. It also keeps narration
// off function_response messages, whose payload records the login URLs a channel
// scrubs out of prose: narration is emitted first, so a message mixing a call
// with a response would post an unscrubbed link. Only ADK emitting tool results
// as their own data-only events rules that shape out — preserve the exclusion if
// widening this gate. A request for confirmation never reaches this path: the
// runtime pauses the task at input-required with the prompt instead.
func narrationDeltas(ev *a2apkg.TaskStatusUpdateEvent, parts a2apkg.ContentParts) []OutboundDelta {
	if !hasFunctionCallPart(parts) || isPartialStatusUpdate(ev) {
		return nil
	}
	text := extractTextFromA2AParts(parts)
	if text == "" {
		return nil
	}
	return []OutboundDelta{{Kind: DeltaNarration, Content: text}}
}

// isPartialStatusUpdate reports whether a status update is a streaming chunk.
// kagent stamps the flag on the event, on its status message, or on both,
// depending on the emitting runtime.
func isPartialStatusUpdate(ev *a2apkg.TaskStatusUpdateEvent) bool {
	if isPartialMeta(ev.Metadata) {
		return true
	}
	return ev.Status.Message != nil && isPartialMeta(ev.Status.Message.Metadata)
}

// toolActivityDeltas maps each function_call/function_response DataPart to a
// DeltaToolActivity, skipping parts that are not tool activity.
func toolActivityDeltas(parts a2apkg.ContentParts) []OutboundDelta {
	var deltas []OutboundDelta
	for _, p := range parts {
		if d := toolActivityDelta(p); !d.isZero() {
			deltas = append(deltas, d)
		}
	}
	return deltas
}

func messageParts(msg *a2apkg.Message) a2apkg.ContentParts {
	if msg == nil {
		return nil
	}
	return msg.Parts
}

// extractTextFromA2AParts concatenates text from A2A content parts.
func extractTextFromA2AParts(parts a2apkg.ContentParts) string {
	var sb bytes.Buffer
	for _, p := range parts {
		if p != nil {
			sb.WriteString(p.Text())
		}
	}
	return sb.String()
}

// sendViaOpenAI POSTs a minimal OpenAI-compat body to the instance and
// streams the SSE response as typed OutboundDelta values.
func (f *Facade) sendViaOpenAI(ctx context.Context, ref InstanceRef, msg InboundMessage) (<-chan OutboundDelta, error) {
	if f == nil || f.Client == nil {
		return nil, errors.New("channels: facade instance client is nil")
	}
	body, err := json.Marshal(map[string]any{
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": msg.Text},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	src, err := f.Client.StreamCompletion(ctx, ref, body)
	if err != nil {
		return nil, err
	}

	out := make(chan OutboundDelta, 16)
	go func() {
		defer close(out)
		defer func() { _ = src.Close() }()

		deltas := make(chan instance.Delta, 16)
		errCh := make(chan error, 1)
		go func() { errCh <- instance.StreamDeltas(ctx, src, deltas) }()

		for d := range deltas {
			if d.Event == "done" || bytes.Equal(bytes.TrimSpace(d.Data), []byte("[DONE]")) {
				select {
				case <-ctx.Done():
				case out <- OutboundDelta{Done: true}:
				}
				continue
			}
			content := extractContent(d.Data)
			if content == "" {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- OutboundDelta{Content: content}:
			}
		}
		if err := <-errCh; err != nil && !errors.Is(err, io.EOF) {
			select {
			case <-ctx.Done():
			case out <- OutboundDelta{Err: err}:
			}
		}
	}()
	return out, nil
}

// FetchHistory returns the stored message log for the thread owned by ref.
func (f *Facade) FetchHistory(ctx context.Context, ref InstanceRef) ([]Message, error) {
	if f == nil || f.Client == nil {
		return nil, errors.New("channels: facade instance client is nil")
	}
	resp, err := f.Client.Messages(ctx, ref, "")
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		out = append(out, Message{Role: m.Role, Content: m.Content})
	}
	return out, nil
}

// extractContent peels the user-visible text out of an OpenAI-style
// `chat.completion.chunk`. Missing fields are treated as empty; channel
// adapters that need the raw SSE should read the stream directly.
func extractContent(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var envelope struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		// Some servers emit a flat {"delta": "..."} shape; tolerate it.
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ""
	}
	if len(envelope.Choices) > 0 {
		if c := envelope.Choices[0].Delta.Content; c != "" {
			return c
		}
		if c := envelope.Choices[0].Message.Content; c != "" {
			return c
		}
	}
	return envelope.Delta
}
