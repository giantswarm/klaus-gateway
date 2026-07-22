package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// KagentClient talks to the kagent controller REST API. It serves two
// concerns that share the same base URL and credentials:
//
// Sessions (Exists, Delete): a GET on a missing session returns 404, which
// the adapter surfaces as "starting fresh", and the session's store-backed
// task list carries the status message of a paused input-required task, from
// which a pending HITL prompt can be rebuilt after a gateway restart.
//
// REST is the only session/task source available. kagent's A2A gateway exposes
// no task listing over the legacy wire klaus-gateway speaks, and sending
// A2A-Version: 1.0 does not help: ListTasks is accepted at the v1 router but
// the gateway's shared passthrough to the v0-pinned agent pod returns
// ErrUnsupportedOperation (kagent-dev/kagent#2187 would serve it from the task
// store instead).
//
// Agents (AgentModel, ListAgents): the per-agent model and the
// installed-agents roster. kagent resolves the model from the agent's
// referenced ModelConfig, so the model fields are populated for declarative
// agents only; a BYO agent yields empty strings (kagent does not know what
// model a bring-your-own runtime uses).
type KagentClient struct {
	// HTTPClient is the HTTP client used for requests. Nil uses a default with a 10-second timeout.
	HTTPClient *http.Client
	// BaseURL is the base URL serving the kagent REST API — the agentgateway
	// endpoint that fronts kagent (e.g. http://agentgateway...:8080/kagent) or
	// the controller directly. "/api/..." paths are appended.
	BaseURL string
	// TokenSource yields the Bearer token sent as Authorization. Nil sends no
	// header. For session lookups it must resolve to the same principal the
	// A2A turn forwards, because kagent keys a session lookup on
	// (session_id, user_id).
	TokenSource TokenSource
}

// Exists reports whether the kagent session identified by sessionID exists for
// the caller's principal. 200 -> true, 404 -> false; any other status or a
// transport error is returned as an error so the caller can treat the result as
// indeterminate rather than a definitive "gone".
func (c *KagentClient) Exists(ctx context.Context, sessionID string) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/sessions/"+sessionID)
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
func (c *KagentClient) Delete(ctx context.Context, sessionID string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/api/sessions/"+sessionID)
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

// AgentModel resolves the model id and provider for agentRef
// ("namespace/name"). Empty strings with a nil error mean kagent does not
// expose a model for this agent (BYO runtime).
func (c *KagentClient) AgentModel(ctx context.Context, agentRef string) (model, provider string, err error) {
	namespace, name, ok := strings.Cut(agentRef, "/")
	if !ok || namespace == "" || name == "" {
		return "", "", fmt.Errorf("kagent agents: agent ref %q is not namespace/name", agentRef)
	}

	var payload struct {
		Data struct {
			Model         string `json:"model"`
			ModelProvider string `json:"modelProvider"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/api/agents/"+namespace+"/"+name, &payload); err != nil {
		return "", "", err
	}
	return payload.Data.Model, payload.Data.ModelProvider, nil
}

// AgentInfo describes one agent installed on the kagent controller.
type AgentInfo struct {
	Name        string
	Namespace   string
	Description string
}

// ListAgents returns the agents the kagent controller knows about, the source
// of the Slack /agent roster. Discovery is dynamic: a newly installed Agent CR
// appears here without any gateway change.
func (c *KagentClient) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	var payload struct {
		Data []struct {
			Agent struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Spec struct {
					Description string `json:"description"`
				} `json:"spec"`
			} `json:"agent"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/api/agents", &payload); err != nil {
		return nil, err
	}
	agents := make([]AgentInfo, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.Agent.Metadata.Name == "" {
			continue
		}
		agents = append(agents, AgentInfo{
			Name:        item.Agent.Metadata.Name,
			Namespace:   item.Agent.Metadata.Namespace,
			Description: item.Agent.Spec.Description,
		})
	}
	return agents, nil
}

// getJSON GETs path (relative to BaseURL) and decodes the JSON response into out.
func (c *KagentClient) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path)
	if err != nil {
		return fmt.Errorf("kagent agents: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kagent agents: unexpected status %d", resp.StatusCode)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("kagent agents: decode: %w", err)
	}
	return nil
}

// do issues a bodyless request to path (relative to BaseURL) with the Accept
// header and Bearer token set. The caller owns the response body and wraps
// errors with its own prefix.
func (c *KagentClient) do(ctx context.Context, method, path string) (*http.Response, error) {
	endpoint := trimRightSlash(c.BaseURL) + path

	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenSource != nil {
		token, err := c.TokenSource.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	return httpClient.Do(req)
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
