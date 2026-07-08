package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/instance"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
)

// InstanceClient is the slice of pkg/instance.Client that the Facade needs.
// Tests can inject a fake without standing up an HTTP server.
type InstanceClient interface {
	StreamCompletion(ctx context.Context, ref InstanceRef, body []byte) (io.ReadCloser, error)
	Messages(ctx context.Context, ref InstanceRef, threadID string) (instance.MessagesResponse, error)
}

// ChannelExecutor is the slice of pkg/a2a.ForwardingExecutor that the Facade
// needs to route channel turns through the A2A path. When nil, SendCompletion
// falls back to the OpenAI /v1 path unconditionally.
type ChannelExecutor interface {
	Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2apkg.Event, error]
}

// SessionExistsChecker reports whether a kagent session exists, keyed by the
// synthesized contextID. Implemented by pkg/a2a.SessionsClient; nil disables
// the resume existence-check.
type SessionExistsChecker interface {
	Exists(ctx context.Context, sessionID string) (bool, error)
}

// Facade wires the routing.Router, instance.Client, and lifecycle.Manager
// together into the Gateway surface used by channel adapters.
type Facade struct {
	Router    *routing.Router
	Client    InstanceClient
	Lifecycle lifecycle.Manager
	// Executor, when non-nil, routes channel turns through the A2A executor
	// instead of the OpenAI /v1 path when InboundMessage.AgentRef is set.
	Executor ChannelExecutor
	// Sessions, when non-nil, backs SessionResumable. Nil leaves the check
	// unavailable (SessionResumable reports checked=false).
	Sessions SessionExistsChecker
}

// SessionResumable reports whether the kagent session for msg's thread already
// exists, so a reply resumes it rather than starting fresh. checked is false
// when no session client is configured or the lookup errored; exists is then
// meaningless and the caller should stay silent. The caller's forwarded token
// is seeded so kagent resolves the same principal the A2A turn will (the lookup
// keys on the session's user_id).
func (f *Facade) SessionResumable(ctx context.Context, msg InboundMessage) (exists, checked bool) {
	if f == nil || f.Sessions == nil {
		return false, false
	}
	contextID := SynthesizeContextID(msg.Channel, msg.ChannelID, msg.UserID, msg.ThreadID, msg.AgentRef)
	ctx = withChannelAuth(ctx, msg)
	ok, err := f.Sessions.Exists(ctx, contextID)
	if err != nil {
		return false, false
	}
	return ok, true
}

// Resolve maps an InboundMessage to a live InstanceRef via the routing
// table (creating a new instance on miss when the router has auto-create
// enabled). On the A2A path (Executor set and AgentRef non-empty) routing is
// bypassed and a zero InstanceRef is returned — the A2A executor routes
// directly to the orchestrator without provisioning a Klaus instance.
func (f *Facade) Resolve(ctx context.Context, in InboundMessage) (InstanceRef, error) {
	if f == nil || f.Router == nil {
		return InstanceRef{}, errors.New("channels: facade router is nil")
	}
	if f.Executor != nil && in.AgentRef != "" {
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

// SendCompletion streams a completion for msg. When Executor is set and
// msg.AgentRef is non-empty, the turn is routed through the A2A executor;
// otherwise it falls back to the OpenAI /v1 SSE path.
//
// The caller must receive from the returned channel until it closes.
func (f *Facade) SendCompletion(ctx context.Context, ref InstanceRef, msg InboundMessage) (<-chan OutboundDelta, error) {
	if f.Executor != nil && msg.AgentRef != "" {
		return f.sendViaA2A(ctx, msg)
	}
	return f.sendViaOpenAI(ctx, ref, msg)
}

// sendViaA2A drives ForwardingExecutor and maps A2A events to OutboundDelta.
func (f *Facade) sendViaA2A(ctx context.Context, msg InboundMessage) (<-chan OutboundDelta, error) {
	contextID := SynthesizeContextID(msg.Channel, msg.ChannelID, msg.UserID, msg.ThreadID, msg.AgentRef)

	taskID := a2apkg.NewTaskID()
	if msg.TaskID != "" {
		taskID = a2apkg.TaskID(msg.TaskID)
	}
	execCtx := &a2asrv.ExecutorContext{
		ContextID: contextID,
		TaskID:    taskID,
		Message:   a2apkg.NewMessage(a2apkg.MessageRoleUser, buildInboundParts(msg)...),
	}

	ctx = withChannelAuth(ctx, msg)

	out := make(chan OutboundDelta, 16)
	go func() {
		defer close(out)
		terminated := false
		for event, err := range f.Executor.Execute(ctx, execCtx) {
			var deltas []OutboundDelta
			if err != nil {
				deltas = []OutboundDelta{{Err: err}}
			} else {
				deltas = mapA2AEvent(event)
			}
			for _, delta := range deltas {
				if delta.isZero() {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case out <- delta:
				}
				if delta.Err != nil || delta.Done || delta.Kind == DeltaPrompt {
					terminated = true
					break
				}
			}
			if terminated {
				break
			}
		}
		if !terminated {
			select {
			case <-ctx.Done():
			case out <- OutboundDelta{Err: errors.New("a2a: stream ended without terminal status")}:
			}
		}
	}()
	return out, nil
}

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
// function_call/function_response DataParts.
func mapA2AEvent(event a2apkg.Event) []OutboundDelta {
	switch ev := event.(type) {
	case *a2apkg.TaskArtifactUpdateEvent:
		if ev.Artifact == nil {
			return nil
		}
		return append(textDelta(ev.Artifact.Parts), toolActivityDeltas(ev.Artifact.Parts)...)
	case *a2apkg.TaskStatusUpdateEvent:
		var usage *TurnUsage
		if !isPartialMeta(ev.Metadata) {
			usage = parseTurnUsage(ev.Metadata)
			if usage == nil && ev.Status.Message != nil {
				usage = parseTurnUsage(ev.Status.Message.Metadata)
			}
		}
		switch ev.Status.State {
		case a2apkg.TaskStateCompleted:
			return []OutboundDelta{{Done: true, Usage: usage}}
		case a2apkg.TaskStateInputRequired, a2apkg.TaskStateAuthRequired:
			var hitl *HitlPrompt
			text := ""
			if ev.Status.Message != nil {
				hitl = parseHitlPrompt(ev.Status.Message)
				text = extractTextFromA2AParts(ev.Status.Message.Parts)
			}
			// kagent carries the prompt in a DataPart, so the text is usually
			// empty; fall back to the structured hint for any plain-text
			// renderer.
			if text == "" && hitl != nil {
				text = hitl.summary()
			}
			return []OutboundDelta{{Kind: DeltaPrompt, Content: text, TaskID: string(ev.TaskID), Prompt: hitl, Usage: usage}}
		default:
			if ev.Status.State.Terminal() {
				msg := fmt.Sprintf("a2a: task ended with state %s", ev.Status.State)
				if ev.Status.Message != nil {
					if text := extractTextFromA2AParts(ev.Status.Message.Parts); text != "" {
						msg = text
					}
				}
				return []OutboundDelta{{Err: errors.New(msg), Usage: usage}}
			}
			// Interim working event: surface any tool activity, and a usage-only
			// delta when the event reports usage.
			deltas := toolActivityDeltas(messageParts(ev.Status.Message))
			if usage != nil {
				deltas = append(deltas, OutboundDelta{Usage: usage})
			}
			return deltas
		}
	}
	return nil
}

// textDelta returns a single-element slice with the concatenated text of parts,
// or nil when there is no text.
func textDelta(parts a2apkg.ContentParts) []OutboundDelta {
	if text := extractTextFromA2AParts(parts); text != "" {
		return []OutboundDelta{{Content: text}}
	}
	return nil
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
