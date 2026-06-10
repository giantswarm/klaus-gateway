package a2a

import (
	"context"
	"fmt"
	"iter"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// agentRefKey is the context key for the target agentRef.
type agentRefKey struct{}

// WithAgentRef stores agentRef in ctx.
func WithAgentRef(ctx context.Context, agentRef string) context.Context {
	return context.WithValue(ctx, agentRefKey{}, agentRef)
}

// AgentRefFromContext returns the agentRef stored by WithAgentRef, or empty string.
func AgentRefFromContext(ctx context.Context) string {
	ref, _ := ctx.Value(agentRefKey{}).(string)
	return ref
}

// A2AClient forwards channel turns to an A2A orchestrator endpoint.
// It satisfies the channels.ChannelExecutor interface.
type A2AClient struct {
	Clients      *Clients
	BaseURL      string // e.g. http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a/kagent
	DefaultAgent string
}

// Execute sends the inbound message via A2A streaming and yields events as
// received. The target agent is read from ctx via WithAgentRef, falling back
// to DefaultAgent.
func (k *A2AClient) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2apkg.Event, error] {
	return func(yield func(a2apkg.Event, error) bool) {
		agentRef := AgentRefFromContext(ctx)
		if agentRef == "" {
			agentRef = k.DefaultAgent
		}
		targetURL := k.BaseURL + "/" + agentRef
		client, err := k.Clients.For(ctx, targetURL)
		if err != nil {
			yield(nil, fmt.Errorf("dial a2a: %w", err))
			return
		}
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
		for event, err := range client.SendStreamingMessage(ctx, params) {
			if !yield(event, err) {
				return
			}
		}
	}
}
