package a2a

import (
	"context"
)

// forwardedTokenKey is the context key for a caller-forwarded bearer token.
type forwardedTokenKey struct{}

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

// WithForwardedToken stores a caller bearer token in ctx for the A2A egress
// request. An empty token leaves ctx unchanged.
func WithForwardedToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, forwardedTokenKey{}, token)
}

// ForwardedTokenFromContext returns the token stored by WithForwardedToken, or
// an empty string.
func ForwardedTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(forwardedTokenKey{}).(string)
	return token
}
