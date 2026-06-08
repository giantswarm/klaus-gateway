package a2a

import (
	"context"
	"fmt"
	"sync"

	a2aclient "github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2a"
)

// Clients caches one a2aclient.Client per target base URL.
// It is safe for concurrent use.
type Clients struct {
	mu      sync.Mutex
	clients map[string]*a2aclient.Client
}

// NewClients returns an empty client cache.
func NewClients() *Clients {
	return &Clients{clients: make(map[string]*a2aclient.Client)}
}

// For returns a cached client for the given base URL, creating one if necessary.
// baseURL must be a valid absolute HTTP(S) URL pointing to the A2A endpoint of
// the target Klaus pod (e.g. "http://Klaus-svc:8080").
func (c *Clients) For(ctx context.Context, baseURL string) (*a2aclient.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if cl, ok := c.clients[baseURL]; ok {
		return cl, nil
	}

	endpoints := []a2a.AgentInterface{
		{Transport: a2a.TransportProtocolJSONRPC, URL: baseURL + "/a2a"},
	}
	cl, err := a2aclient.NewFromEndpoints(ctx, endpoints)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", baseURL, err)
	}
	c.clients[baseURL] = cl
	return cl, nil
}
