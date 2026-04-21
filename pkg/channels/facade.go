package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

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

// Facade wires the routing.Router, instance.Client, and lifecycle.Manager
// together into the Gateway surface used by channel adapters.
type Facade struct {
	Router    *routing.Router
	Client    InstanceClient
	Lifecycle lifecycle.Manager
}

// Resolve maps an InboundMessage to a live InstanceRef via the routing
// table (creating a new instance on miss when the router has auto-create
// enabled).
func (f *Facade) Resolve(ctx context.Context, in InboundMessage) (InstanceRef, error) {
	if f == nil || f.Router == nil {
		return InstanceRef{}, errors.New("channels: facade router is nil")
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

// SendCompletion POSTs a minimal OpenAI-compat body to the instance and
// streams the SSE response as typed OutboundDelta values.
//
// The caller must receive from the returned channel until it closes; the
// stream goroutine exits when the upstream body ends, the context is
// cancelled, or a send fails.
func (f *Facade) SendCompletion(ctx context.Context, ref InstanceRef, msg InboundMessage) (<-chan OutboundDelta, error) {
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
		defer src.Close()

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
