package a2a

import (
	"context"
)

// forwardedTokenKey is the context key for a caller-forwarded bearer token.
type forwardedTokenKey struct{}

// shareTokenKey is the context key for a Session share token.
type shareTokenKey struct{}

// agentRefKey is the context key for the target agentRef: the Agent an A2A
// call names as its tenant.
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

// WithShareToken stores a Session share token in ctx. Every kagent call
// made with ctx then carries it next to the caller's own bearer, which lets a
// caller who is not the session's creator act on that one session as
// themselves. An empty token leaves ctx unchanged.
func WithShareToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, shareTokenKey{}, token)
}

// ShareTokenFromContext returns the token stored by WithShareToken, or an
// empty string.
func ShareTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(shareTokenKey{}).(string)
	return token
}
