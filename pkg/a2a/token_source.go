package a2a

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// forwardedTokenKey is the context key for a caller-forwarded bearer token.
type forwardedTokenKey struct{}

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
// token it defers to Fallback (e.g. a ServiceAccount token for channels that
// have no per-user token, such as Slack). Fallback may be nil.
type ForwardedTokenSource struct {
	Fallback TokenSource
}

// Token prefers the forwarded caller token, falling back to Fallback.
func (s ForwardedTokenSource) Token(ctx context.Context) (string, error) {
	if token := ForwardedTokenFromContext(ctx); token != "" {
		return token, nil
	}
	if s.Fallback != nil {
		return s.Fallback.Token(ctx)
	}
	return "", nil
}
