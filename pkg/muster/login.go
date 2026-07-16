package muster

import (
	"context"
	"fmt"
	"regexp"
)

// Tool names on muster's aggregator for manual backend login/logout.
const (
	loginTool  = "core_auth_login"
	logoutTool = "core_auth_logout"
)

// authURLPattern extracts the authorization URL from core_auth_login's
// human-readable text result; the tool exposes no structured URL field.
var authURLPattern = regexp.MustCompile(`https://[^\s<>()"']+`)

// NoLoginURLError is returned when core_auth_login answered without an
// authorization URL: already authenticated, an SSO server (manual login
// rejected), rate-limited, or a discovery failure. Text carries muster's
// full reply for logging.
type NoLoginURLError struct {
	Server string
	Text   string
}

func (e *NoLoginURLError) Error() string {
	return fmt.Sprintf("muster: core_auth_login for %s returned no authorization URL: %s", e.Server, e.Text)
}

// LoginURL starts a manual login for the named backend on behalf of the
// bearer and returns the authorization URL to hand to the user. The URL
// carries a single-use state bound to the caller's session with a short
// expiry (10 minutes on muster), so fetch it only when about to present it.
func (c *Client) LoginURL(ctx context.Context, token, server string) (string, error) {
	result, err := c.CallTool(ctx, token, loginTool, map[string]any{"server": server})
	if err != nil {
		return "", err
	}
	if url := authURLPattern.FindString(result.Text); url != "" && !result.IsError {
		return url, nil
	}
	return "", &NoLoginURLError{Server: server, Text: result.Text}
}

// Logout disconnects the named backend for the bearer's session via
// core_auth_logout.
func (c *Client) Logout(ctx context.Context, token, server string) error {
	result, err := c.CallTool(ctx, token, logoutTool, map[string]any{"server": server})
	if err != nil {
		return err
	}
	if result.IsError {
		return fmt.Errorf("muster: core_auth_logout for %s: %s", server, result.Text)
	}
	return nil
}
