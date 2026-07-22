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

// AgentsClient reads agent metadata from the kagent controller REST API: the
// per-agent model (AgentModel) and the installed-agents roster (ListAgents).
// kagent resolves the model from the agent's referenced ModelConfig, so the
// model fields are populated for declarative agents only; a BYO agent yields
// empty strings (kagent does not know what model a bring-your-own runtime uses).
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
func (c *AgentsClient) ListAgents(ctx context.Context) ([]AgentInfo, error) {
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
func (c *AgentsClient) getJSON(ctx context.Context, path string, out any) error {
	endpoint := trimRightSlash(c.BaseURL) + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("kagent agents: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenSource != nil {
		token, err := c.TokenSource.Token(ctx)
		if err != nil {
			return fmt.Errorf("kagent agents: token: %w", err)
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
