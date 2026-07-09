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

// AgentsClient reads agent metadata from the kagent controller REST API.
// kagent resolves the model from the agent's referenced ModelConfig, so the
// fields are populated for declarative agents only; a BYO agent yields empty
// strings (kagent does not know what model a bring-your-own runtime uses).
type AgentsClient struct {
	// HTTPClient is the HTTP client used for requests. Nil uses a default with a 10-second timeout.
	HTTPClient *http.Client
	// BaseURL is the base URL serving the kagent REST API; "/api/agents/{namespace}/{name}" is appended.
	BaseURL string
	// TokenSource yields the Bearer token sent as Authorization. Nil sends no header.
	TokenSource TokenSource
}

// AgentModel resolves the model id and provider for agentRef
// ("namespace/name"). Empty strings with a nil error mean kagent does not
// expose a model for this agent (BYO runtime).
func (c *AgentsClient) AgentModel(ctx context.Context, agentRef string) (model, provider string, err error) {
	namespace, name, ok := strings.Cut(agentRef, "/")
	if !ok || namespace == "" || name == "" {
		return "", "", fmt.Errorf("kagent agents: agent ref %q is not namespace/name", agentRef)
	}
	endpoint := trimRightSlash(c.BaseURL) + "/api/agents/" + namespace + "/" + name

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", fmt.Errorf("kagent agents: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenSource != nil {
		token, err := c.TokenSource.Token(ctx)
		if err != nil {
			return "", "", fmt.Errorf("kagent agents: token: %w", err)
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
		return "", "", fmt.Errorf("kagent agents: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("kagent agents: unexpected status %d", resp.StatusCode)
	}

	var payload struct {
		Data struct {
			Model         string `json:"model"`
			ModelProvider string `json:"modelProvider"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", "", fmt.Errorf("kagent agents: decode: %w", err)
	}
	return payload.Data.Model, payload.Data.ModelProvider, nil
}
