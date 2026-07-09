package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	a2apkg "github.com/a2aproject/a2a-go/v2/a2a"
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

// PendingTask returns the id and input-required status message of the
// session's paused task, straight from kagent's task store
// (GET /api/sessions/{id}/tasks). An empty taskID with a nil error means
// nothing is pending: the session does not exist (404), has no tasks, or its
// latest input-required task was already resolved. The endpoint serves the
// legacy (spec-lowercase) task JSON, decoded with the same bridge types the
// SSE stream uses.
func (c *SessionsClient) PendingTask(ctx context.Context, sessionID string) (taskID string, statusMessage *a2apkg.Message, err error) {
	endpoint := trimRightSlash(c.BaseURL) + "/api/sessions/" + sessionID + "/tasks"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", nil, fmt.Errorf("kagent session tasks: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenSource != nil {
		token, err := c.TokenSource.Token(ctx)
		if err != nil {
			return "", nil, fmt.Errorf("kagent session tasks: token: %w", err)
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
		return "", nil, fmt.Errorf("kagent session tasks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", nil, nil
	default:
		return "", nil, fmt.Errorf("kagent session tasks: unexpected status %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID     string        `json:"id"`
			Status *kagentStatus `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return "", nil, fmt.Errorf("kagent session tasks: decode: %w", err)
	}

	// Tasks are listed in creation order and only the most recent one can still
	// be waiting on input; an older input-required task that was abandoned for a
	// new turn must not be resumed (its confirmation no longer matches the model
	// history).
	if len(payload.Data) == 0 {
		return "", nil, nil
	}
	last := payload.Data[len(payload.Data)-1]
	if last.Status == nil || mapKagentState(last.Status.State) != a2apkg.TaskStateInputRequired {
		return "", nil, nil
	}
	return last.ID, last.Status.Message, nil
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
