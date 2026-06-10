package a2a

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2aclient "github.com/a2aproject/a2a-go/v2/a2aclient"
)

// Clients caches one a2aclient.Client per target base URL.
// It is safe for concurrent use.
type Clients struct {
	mu        sync.RWMutex
	clients   map[string]*a2aclient.Client
	tokenPath string // path to a Bearer token file; empty = unauthenticated
}

// NewClients returns an empty client cache. tokenPath is an optional path to a
// file containing a Bearer token injected on every outgoing request; pass ""
// for unauthenticated connections (in-cluster open endpoints).
func NewClients(tokenPath string) *Clients {
	return &Clients{clients: make(map[string]*a2aclient.Client), tokenPath: tokenPath}
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

	opts := []a2aclient.FactoryOption{}
	if c.tokenPath != "" {
		opts = append(opts, a2aclient.WithCallInterceptors(&bearerFileInterceptor{path: c.tokenPath}))
	}

	cl, err := a2aclient.NewFromEndpoints(ctx, endpoints, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", baseURL, err)
	}
	c.clients[baseURL] = cl
	return cl, nil
}

// bearerFileInterceptor reads a Bearer token from a file on each request and
// injects it as the Authorization header. The token is re-read each call so
// rotation (projected SA tokens refresh every hour) is picked up automatically.
type bearerFileInterceptor struct {
	a2aclient.PassthroughInterceptor
	path string
}

func (b *bearerFileInterceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	token, err := os.ReadFile(b.path)
	if err != nil {
		return ctx, nil, fmt.Errorf("read bearer token %s: %w", b.path, err)
	}
	req.ServiceParams.Append("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	return ctx, nil, nil
}
