package a2a

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A failed card fetch is negative-cached: a second lookup within the TTL does
// not hit the server, and a lookup after the TTL retries (and can succeed).
func TestCardIdentity_NegativeCachesFetchFailures(t *testing.T) {
	var hits atomic.Int64
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"name":"SRE agent"}`))
	}))
	t.Cleanup(server.Close)

	client := &AgentCardClient{BaseURL: server.URL}

	name, iconURL := client.CardIdentity(t.Context(), "sre-agent")
	require.Empty(t, name)
	require.Empty(t, iconURL)
	require.EqualValues(t, 1, hits.Load())

	name, _ = client.CardIdentity(t.Context(), "sre-agent")
	require.Empty(t, name)
	require.EqualValues(t, 1, hits.Load(), "a lookup within the failure TTL must not hit the server")

	// Expire the negative entry and let the server recover: the next lookup
	// retries and caches the card.
	client.mu.Lock()
	client.failedUntil["sre-agent"] = time.Now().Add(-time.Second)
	client.mu.Unlock()
	fail.Store(false)

	name, _ = client.CardIdentity(t.Context(), "sre-agent")
	require.Equal(t, "SRE agent", name)
	require.EqualValues(t, 2, hits.Load(), "an expired negative entry retries the fetch")

	name, _ = client.CardIdentity(t.Context(), "sre-agent")
	require.Equal(t, "SRE agent", name)
	require.EqualValues(t, 2, hits.Load(), "a successful fetch is cached")
}
