package a2a

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
)

// forwardedTokenKey is the context key for a caller-forwarded bearer token.
type forwardedTokenKey struct{}

// channelKey is the context key for the originating channel name.
type channelKey struct{}

// WithChannel stores the originating channel name (e.g. "slack", "web") in ctx
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

// FileTokenSource reads a bearer token from a file on every call so rotation of
// a projected ServiceAccount token is picked up without a restart.
type FileTokenSource struct {
	Path string
}

// Token reads and trims the file contents. An empty Path yields an empty token.
func (s FileTokenSource) Token(_ context.Context) (string, error) {
	if s.Path == "" {
		return "", nil
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read bearer token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ForwardedTokenSource returns the caller's bearer token from ctx, preserving
// the end-user identity end-to-end through agentgateway. When ctx carries no
// token it defers to Fallback (e.g. a ServiceAccount token for anonymous
// callers). Fallback may be nil.
//
// Channels listed in ForwardedOnlyChannels never use the Fallback: a request
// from one of them without a forwarded token is an error, so an
// identity-bearing channel (Slack with account linking) can never silently
// run a turn as the gateway's machine identity.
type ForwardedTokenSource struct {
	Fallback TokenSource
	// ForwardedOnlyChannels lists channel names (matched against WithChannel)
	// for which a missing forwarded token is a hard error instead of a
	// Fallback lookup.
	ForwardedOnlyChannels []string
}

// Token prefers the forwarded caller token, falling back to Fallback except
// for forwarded-only channels.
func (s ForwardedTokenSource) Token(ctx context.Context) (string, error) {
	if token := ForwardedTokenFromContext(ctx); token != "" {
		return token, nil
	}
	if channel := ChannelFromContext(ctx); slices.Contains(s.ForwardedOnlyChannels, channel) {
		return "", fmt.Errorf("a2a: no forwarded token for channel %q and the service-account fallback is disabled for it", channel)
	}
	if s.Fallback != nil {
		return s.Fallback.Token(ctx)
	}
	return "", nil
}
