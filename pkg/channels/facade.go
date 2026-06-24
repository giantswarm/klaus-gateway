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

// Facade wires the routing.Router, instance.Client, and lifecycle.Manager
// together into the Gateway surface used by channel adapters.
type Facade struct {
	Router    *routing.Router
	Client    InstanceClient
	Lifecycle lifecycle.Manager
	// Executor, when non-nil, routes channel turns through the A2A executor
	// instead of the OpenAI /v1 path when InboundMessage.AgentRef is set.
	Executor ChannelExecutor
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

	execCtx := &a2asrv.ExecutorContext{
		ContextID: contextID,
		TaskID:    a2apkg.NewTaskID(),
		Message:   a2apkg.NewMessage(a2apkg.MessageRoleUser, a2apkg.NewTextPart(msg.Text)),
	}

	ctx = withChannelAuth(ctx, msg)

	out := make(chan OutboundDelta, 16)
	go func() {
		defer close(out)
		terminated := false
		for event, err := range f.Executor.Execute(ctx, execCtx) {
			var delta OutboundDelta
			switch {
			case err != nil:
				delta = OutboundDelta{Err: err}
			default:
				delta = mapA2AEvent(event)
			}
			if delta == (OutboundDelta{}) {
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
		if !terminated {
			select {
			case <-ctx.Done():
			case out <- OutboundDelta{Err: errors.New("a2a: stream ended without terminal status")}:
			}
		}
	}()
	return out, nil
}

// withChannelAuth seeds ctx with the target agent ref and the caller's
// forwarded bearer token for the A2A client.
func withChannelAuth(ctx context.Context, msg InboundMessage) context.Context {
	ctx = pkga2a.WithAgentRef(ctx, msg.AgentRef)
	ctx = pkga2a.WithForwardedToken(ctx, msg.BearerToken)
	return ctx
}

// mapA2AEvent converts a single A2A streaming event to an OutboundDelta.
// Returns a zero value for events that carry no channel-visible payload.
// Non-completed terminal states (failed, rejected, canceled) are mapped to an
// error delta so channels surface them rather than silently closing.
func mapA2AEvent(event a2apkg.Event) OutboundDelta {
	switch ev := event.(type) {
	case *a2apkg.TaskArtifactUpdateEvent:
		if ev.Artifact == nil {
			return OutboundDelta{}
		}
		text := extractTextFromA2AParts(ev.Artifact.Parts)
		if text == "" {
			return OutboundDelta{}
		}
		return OutboundDelta{Content: text}
	case *a2apkg.TaskStatusUpdateEvent:
		switch ev.Status.State {
		case a2apkg.TaskStateCompleted:
			return OutboundDelta{Done: true}
		case a2apkg.TaskStateInputRequired, a2apkg.TaskStateAuthRequired:
			prompt := ""
			if ev.Status.Message != nil {
				prompt = extractTextFromA2AParts(ev.Status.Message.Parts)
			}
			return OutboundDelta{Kind: DeltaPrompt, Content: prompt}
		default:
			if ev.Status.State.Terminal() {
				msg := fmt.Sprintf("a2a: task ended with state %s", ev.Status.State)
				if ev.Status.Message != nil {
					if text := extractTextFromA2AParts(ev.Status.Message.Parts); text != "" {
						msg = text
					}
				}
				return OutboundDelta{Err: errors.New(msg)}
			}
		}
	}
	return OutboundDelta{}
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
