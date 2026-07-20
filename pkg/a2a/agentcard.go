package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// AgentCardClient fetches and caches an agent's A2A AgentCard from the
// well-known endpoint {BaseURL}/{agentRef}/.well-known/agent-card.json. The card
// is the backend-neutral source of an agent's display name and icon (kagent
// serves iconUrl from the Agent CRD spec since kagent-dev/kagent#2188; a
// backend that does not populate one yields an empty IconURL).
type AgentCardClient struct {
	// HTTPClient is the HTTP client used for requests. Nil uses a default with a 10-second timeout.
	HTTPClient *http.Client
	// BaseURL is the A2A base fronting the agents (the same agentgateway endpoint
	// the executor posts to). "/{agentRef}/.well-known/agent-card.json" is appended.
	BaseURL string
	// TokenSource yields the Bearer token for the card request. Nil sends none.
	TokenSource TokenSource

	mu    sync.Mutex
	cache map[string]*a2a.AgentCard
	// failedUntil is the per-agent negative cache: no re-fetch before the
	// deadline. The card is resolved synchronously on the dispatch path, so
	// without it every turn against an unreachable card endpoint pays the full
	// HTTP client timeout.
	failedUntil map[string]time.Time
}

// cardFailureTTL is how long a failed card fetch is remembered before the
// endpoint is tried again.
const cardFailureTTL = 45 * time.Second

// CardIdentity returns the agentRef's card display name and icon URL, fetching
// and caching the card on first use. Any error yields empty strings, so the
// caller falls back to config or the app default; branding never blocks a turn.
func (c *AgentCardClient) CardIdentity(ctx context.Context, agentRef string) (username, iconURL string) {
	card, err := c.card(ctx, agentRef)
	if err != nil || card == nil {
		return "", ""
	}
	return card.Name, card.IconURL
}

func (c *AgentCardClient) card(ctx context.Context, agentRef string) (*a2a.AgentCard, error) {
	c.mu.Lock()
	if card, ok := c.cache[agentRef]; ok {
		c.mu.Unlock()
		return card, nil
	}
	if until, ok := c.failedUntil[agentRef]; ok && time.Now().Before(until) {
		c.mu.Unlock()
		return nil, fmt.Errorf("agent card: recent fetch failure for %q, not retrying before %s", agentRef, until.Format(time.RFC3339))
	}
	c.mu.Unlock()

	card, err := c.fetchCard(ctx, agentRef)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.failedUntil == nil {
			c.failedUntil = make(map[string]time.Time)
		}
		c.failedUntil[agentRef] = time.Now().Add(cardFailureTTL)
		return nil, err
	}
	if c.cache == nil {
		c.cache = make(map[string]*a2a.AgentCard)
	}
	c.cache[agentRef] = card
	delete(c.failedUntil, agentRef)
	return card, nil
}

func (c *AgentCardClient) fetchCard(ctx context.Context, agentRef string) (*a2a.AgentCard, error) {
	endpoint := trimRightSlash(c.BaseURL) + "/" + agentRef + "/.well-known/agent-card.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("agent card: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenSource != nil {
		token, err := c.TokenSource.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("agent card: token: %w", err)
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
		return nil, fmt.Errorf("agent card: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent card: unexpected status %d", resp.StatusCode)
	}

	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return nil, fmt.Errorf("agent card: decode: %w", err)
	}
	return &card, nil
}
