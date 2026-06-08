// Package kagentapi provides a client for the kagent HTTP API.
//
// The client pushes A2A turn data (tasks and session events) to kagent so
// the UI can show conversation history for BYO agents routed through the
// gateway. Authentication credentials are passed explicitly on each call;
// no context keys are used.
package kagentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// AuthInfo carries the identity that the gateway passes to kagent on behalf
// of the inbound request. BearerToken is the raw token forwarded from the
// downstream caller; UserSub is the subject extracted from the
// agentgateway-validated X-User-Id header.
type AuthInfo struct {
	BearerToken string
	UserSub     string
}

// SessionEvent is the payload for POST /api/sessions/{sessionID}/events.
type SessionEvent struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

// a2aMessage is the wire shape for the Data field inside a SessionEvent.
type a2aMessage struct {
	Kind      string    `json:"kind"`
	MessageID string    `json:"messageId"`
	Role      string    `json:"role"`
	Parts     []a2aPart `json:"parts"`
}

type a2aPart struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type a2aTask struct {
	Kind      string        `json:"kind"`
	ID        string        `json:"id"`
	ContextID string        `json:"contextId"`
	Status    a2aTaskStatus `json:"status"`
	History   []a2aMessage  `json:"history,omitempty"`
}

type a2aTaskStatus struct {
	State string `json:"state"`
}

const (
	kindMessage = "message"
	kindText    = "text"
)

// NewSessionEvent builds a SessionEvent encoding a single-part text message.
// id is a unique event identifier; role is "user" or "agent".
func NewSessionEvent(id, role, text string) SessionEvent {
	msg := a2aMessage{
		Kind:      kindMessage,
		MessageID: id,
		Role:      role,
		Parts:     []a2aPart{{Kind: kindText, Text: text}},
	}
	data, _ := json.Marshal(msg)
	return SessionEvent{ID: id, Data: string(data)}
}

const defaultSATokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // path, not a credential

// Client sends turn data to the kagent API. A zero-endpoint Client is a
// no-op; callers do not need to guard on Enabled().
type Client struct {
	endpoint    string
	agentRef    string
	httpClient  *http.Client
	saTokenPath string

	saTokenMu     sync.RWMutex
	saTokenCached string
	saTokenExpiry time.Time
}

// New returns a Client targeting the given endpoint. endpoint may be empty,
// in which case all operations are silently skipped. agentRef is the value
// sent in the X-Agent-Name header and comes from KAGENT_AGENT_REF.
func New(endpoint, agentRef string) *Client {
	return &Client{
		endpoint:    endpoint,
		agentRef:    agentRef,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		saTokenPath: defaultSATokenPath,
	}
}

// Enabled reports whether the client has an endpoint configured.
func (c *Client) Enabled() bool {
	return c.endpoint != ""
}

// PushEvent posts a single session event to kagent. The call is best-effort:
// errors are logged and not returned to avoid blocking the A2A response stream.
func (c *Client) PushEvent(ctx context.Context, sessionID string, event SessionEvent, auth AuthInfo) {
	if !c.Enabled() {
		return
	}
	body, err := json.Marshal(event)
	if err != nil {
		slog.ErrorContext(ctx, "kagentapi: marshal event", "error", err)
		return
	}
	endpoint := fmt.Sprintf("%s/api/sessions/%s/events", c.endpoint, url.PathEscape(sessionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		slog.ErrorContext(ctx, "kagentapi: build event request", "error", err)
		return
	}
	c.setHeaders(req, auth)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "kagentapi: push event", "error", err, "session", sessionID)
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		slog.WarnContext(ctx, "kagentapi: push event non-2xx", "status", resp.StatusCode, "session", sessionID)
	}
}

// StoreTask posts the completed turn as an A2A task object to kagent.
// state is the terminal A2A task state (e.g. "completed", "failed").
// The call is best-effort; errors are logged only.
func (c *Client) StoreTask(ctx context.Context, taskID, contextID, userText, agentText, state string, auth AuthInfo) {
	if !c.Enabled() {
		return
	}
	task := a2aTask{
		Kind:      "task",
		ID:        taskID,
		ContextID: contextID,
		Status:    a2aTaskStatus{State: state},
		History: []a2aMessage{
			{Kind: kindMessage, MessageID: taskID + "-user", Role: "user", Parts: []a2aPart{{Kind: kindText, Text: userText}}},
			{Kind: kindMessage, MessageID: taskID + "-agent", Role: "agent", Parts: []a2aPart{{Kind: kindText, Text: agentText}}},
		},
	}
	body, err := json.Marshal(task)
	if err != nil {
		slog.ErrorContext(ctx, "kagentapi: marshal task", "error", err)
		return
	}
	endpoint := fmt.Sprintf("%s/api/tasks", c.endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		slog.ErrorContext(ctx, "kagentapi: build task request", "error", err)
		return
	}
	c.setHeaders(req, auth)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "kagentapi: store task", "error", err, "task", taskID)
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		slog.WarnContext(ctx, "kagentapi: store task non-2xx", "status", resp.StatusCode, "task", taskID)
	}
}

func (c *Client) setHeaders(req *http.Request, auth AuthInfo) {
	req.Header.Set("Content-Type", "application/json")
	token := auth.BearerToken
	if token == "" {
		token = c.readServiceAccountToken()
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if auth.UserSub != "" {
		req.Header.Set("X-User-Id", auth.UserSub)
	}
	if c.agentRef != "" {
		req.Header.Set("X-Agent-Name", c.agentRef)
	}
}

func (c *Client) readServiceAccountToken() string {
	if c.saTokenPath == "" {
		return ""
	}
	c.saTokenMu.RLock()
	if time.Now().Before(c.saTokenExpiry) {
		token := c.saTokenCached
		c.saTokenMu.RUnlock()
		return token
	}
	c.saTokenMu.RUnlock()

	c.saTokenMu.Lock()
	defer c.saTokenMu.Unlock()
	if time.Now().Before(c.saTokenExpiry) {
		return c.saTokenCached
	}
	data, err := os.ReadFile(c.saTokenPath)
	if err != nil {
		return ""
	}
	c.saTokenCached = strings.TrimSpace(string(data))
	c.saTokenExpiry = time.Now().Add(5 * time.Minute)
	return c.saTokenCached
}
