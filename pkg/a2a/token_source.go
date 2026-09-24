package a2a

import (
	"context"
	"fmt"
	"slices"
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

// channelKey is the context key for the originating channel name.
type channelKey struct{}

// WithChannel stores the originating channel name (e.g. "slack") in ctx
// for the A2A egress request. An empty channel leaves ctx unchanged.
func WithChannel(ctx context.Context, channel string) context.Context {
	if channel == "" {
		return ctx
	}
	return context.WithValue(ctx, channelKey{}, channel)
}

// ChannelFromContext returns the channel name stored by WithChannel, or an
// empty string.
func ChannelFromContext(ctx context.Context) string {
	channel, _ := ctx.Value(channelKey{}).(string)
	return channel
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

// TokenSource yields the bearer token for an outgoing A2A request. An empty
// token with a nil error sends the request without an Authorization header.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// ForwardedTokenSource returns the caller's bearer token from ctx, preserving
// the end-user identity end-to-end through agentgateway. A request without a
// forwarded token goes without an Authorization header, except from a channel
// listed in ForwardedOnlyChannels: there it is an error, so an identity-bearing
// channel (Slack with account linking) can never run a turn without the
// person's identity.
type ForwardedTokenSource struct {
	// ForwardedOnlyChannels lists channel names (matched against WithChannel)
	// for which a missing forwarded token is an error.
	ForwardedOnlyChannels []string
}

// Token returns the forwarded caller token, an error for a forwarded-only
// channel without one, and an empty token otherwise.
func (s ForwardedTokenSource) Token(ctx context.Context) (string, error) {
	if token := ForwardedTokenFromContext(ctx); token != "" {
		return token, nil
	}
	if channel := ChannelFromContext(ctx); slices.Contains(s.ForwardedOnlyChannels, channel) {
		return "", fmt.Errorf("a2a: no forwarded token for channel %q", channel)
	}
	return "", nil
}
