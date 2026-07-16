package muster

import (
	"context"
	"encoding/json"
	"fmt"
)

// AuthStatusURI is muster's per-session authentication status resource.
const AuthStatusURI = "auth://status"

// Server status values muster reports per backend.
const (
	StatusConnected     = "connected"
	StatusAuthRequired  = "auth_required"
	StatusReauthRequired = "reauth_required"
)

// ServerStatus is one backend's entry in muster's auth://status resource
// (muster pkg/oauth.ServerAuthStatus).
type ServerStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Issuer string `json:"issuer,omitempty"`
	Scope  string `json:"scope,omitempty"`
	// AuthTool names the tool that starts a manual login (core_auth_login).
	// muster sets it only for connector-style backends whose credential the
	// user must grant via the backend's own consent flow; forward-token and
	// SSO backends never carry it.
	AuthTool               string `json:"auth_tool,omitempty"`
	Error                  string `json:"error,omitempty"`
	TokenForwardingEnabled bool   `json:"token_forwarding_enabled,omitempty"`
	TokenExchangeEnabled   bool   `json:"token_exchange_enabled,omitempty"`
	SSOAttemptFailed       bool   `json:"sso_attempt_failed,omitempty"`
}

// Connector reports whether the backend is a user-consent connector: one the
// caller logs into via muster's manual login tool.
func (s ServerStatus) Connector() bool { return s.AuthTool != "" }

// NeedsAuth reports whether the backend is waiting on the caller to
// (re-)authenticate.
func (s ServerStatus) NeedsAuth() bool {
	return s.Status == StatusAuthRequired || s.Status == StatusReauthRequired
}

// AuthStatus reads auth://status with the caller's bearer and returns the
// per-backend statuses for that caller's session.
func (c *Client) AuthStatus(ctx context.Context, token string) ([]ServerStatus, error) {
	_, text, err := c.ReadResource(ctx, token, AuthStatusURI)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Servers []ServerStatus `json:"servers"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, fmt.Errorf("muster: decode auth status: %w", err)
	}
	return payload.Servers, nil
}
