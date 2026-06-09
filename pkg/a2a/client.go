package a2a

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2aclient "github.com/a2aproject/a2a-go/v2/a2aclient"
)

// Clients caches one a2aclient.Client per target base URL.
// It is safe for concurrent use.
type Clients struct {
	mu      sync.RWMutex
	clients map[string]*a2aclient.Client
}

// NewClients returns an empty client cache.
func NewClients() *Clients {
	return &Clients{clients: make(map[string]*a2aclient.Client)}
}

// For returns a cached client for the given base URL, creating one if necessary.
// A trailing "/a2a" path segment is stripped from baseURL before lookup so callers
// that include it in their static config don't produce double-path endpoints.
func (c *Clients) For(ctx context.Context, baseURL string) (*a2aclient.Client, error) {
	baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/a2a")

	// Fast path: shared read lock.
	c.mu.RLock()
	cl, ok := c.clients[baseURL]
	c.mu.RUnlock()
	if ok {
		return cl, nil
	}

	// Slow path: exclusive lock with double-check.
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[baseURL]; ok {
		return cl, nil
	}

	endpoints := []*a2a.AgentInterface{
		{
			URL:             baseURL + "/a2a",
			ProtocolBinding: a2a.TransportProtocolJSONRPC,
			ProtocolVersion: a2a.Version,
		},
	}
	cl, err := a2aclient.NewFromEndpoints(ctx, endpoints)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", baseURL, err)
	}
	c.clients[baseURL] = cl
	return cl, nil
}
