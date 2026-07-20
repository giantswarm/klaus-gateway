package a2a

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// SessionsClient reads session state from the kagent controller REST API: a
// GET on a missing session returns 404, which the adapter surfaces as
// "starting fresh", and the session's store-backed task list carries the
// status message of a paused input-required task, from which a pending HITL
// prompt can be rebuilt after a gateway restart.
//
// REST is the only session/task source available. kagent's A2A gateway exposes
// no task listing over the legacy wire klaus-gateway speaks, and sending
// A2A-Version: 1.0 does not help: ListTasks is accepted at the v1 router but
// the gateway's shared passthrough to the v0-pinned agent pod returns
// ErrUnsupportedOperation (kagent-dev/kagent#2187 would serve it from the task
// store instead).
type SessionsClient struct {
	// HTTPClient is the HTTP client used for requests. Nil uses a default with a 10-second timeout.
	HTTPClient *http.Client
	// BaseURL is the base URL serving the kagent REST API — the agentgateway
	// endpoint that fronts kagent (e.g. http://agentgateway...:8080/kagent) or
	// the controller directly. "/api/sessions/{id}" is appended.
	BaseURL string
	// TokenSource yields the Bearer token sent as Authorization. It must resolve
	// to the same principal the A2A turn forwards, because kagent keys a session
	// lookup on (session_id, user_id).
	TokenSource TokenSource
}

// Exists reports whether the kagent session identified by sessionID exists for
// the caller's principal. 200 -> true, 404 -> false; any other status or a
// transport error is returned as an error so the caller can treat the result as
// indeterminate rather than a definitive "gone".
func (c *SessionsClient) Exists(ctx context.Context, sessionID string) (bool, error) {
	endpoint := trimRightSlash(c.BaseURL) + "/api/sessions/" + sessionID

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("kagent sessions: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenSource != nil {
		token, err := c.TokenSource.Token(ctx)
		if err != nil {
			return false, fmt.Errorf("kagent sessions: token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("kagent sessions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("kagent sessions: unexpected status %d", resp.StatusCode)
	}
}

// Delete removes the kagent session identified by sessionID for the caller's
// principal. Used to reset a session whose persisted history the model API
// rejects (e.g. a tool call left without a result by an interrupted turn), so
// the conversation can start fresh instead of failing on every later message.
// A 404 is success: the session is gone either way.
func (c *SessionsClient) Delete(ctx context.Context, sessionID string) error {
	endpoint := trimRightSlash(c.BaseURL) + "/api/sessions/" + sessionID

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("kagent sessions: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenSource != nil {
		token, err := c.TokenSource.Token(ctx)
		if err != nil {
			return fmt.Errorf("kagent sessions: token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kagent sessions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("kagent sessions: unexpected status %d", resp.StatusCode)
	}
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
