package a2a

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// SessionsClient reads session state from the kagent controller REST API. It is
// used to decide whether a Slack thread's session can be resumed: a GET on a
// missing session returns 404, which the adapter surfaces as "starting fresh".
type SessionsClient struct {
	// HTTPClient is the HTTP client used for requests. Nil uses a default with a 10-second timeout.
	HTTPClient *http.Client
	// BaseURL is the kagent controller root, e.g. http://kagent-controller.kagent.svc.cluster.local:8083
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

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
