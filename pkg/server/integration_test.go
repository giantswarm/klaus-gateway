package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/observability"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	boltstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/bolt"
	"github.com/giantswarm/klaus-gateway/pkg/server"
)

// TestIntegration_BootWithBolt exercises the acceptance-criteria bullet:
// server skeleton boots with --store=bolt, /healthz + /readyz respond, and
// /metrics exposes the Go collectors plus the gateway histograms.
func TestIntegration_BootWithBolt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.bolt")
	s, err := boltstore.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Seed state so the store is non-empty when ready probes run.
	k := store.Key{Channel: "web", ChannelID: "c1", UserID: "u1", ThreadID: "t1"}
	require.NoError(t, s.Put(context.Background(), k, store.Entry{
		Instance: "i1", LastSeen: time.Now(), TTL: time.Hour,
	}))

	srv := server.New(server.Options{
		PublicAddress: "127.0.0.1:0",
		AdminAddress:  "127.0.0.1:0",
		Metrics:       observability.NewMetrics(),
		Ready: func(ctx context.Context) error {
			_, err := s.List(ctx)
			return err
		},
	})

	public := httptest.NewServer(srv.PublicHandler())
	t.Cleanup(public.Close)
	admin := httptest.NewServer(srv.AdminHandler())
	t.Cleanup(admin.Close)

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(admin.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, path)
	}

	// Drive the public mux once so the RED histograms have a sample.
	resp, err := http.Get(public.URL + "/warmup")
	require.NoError(t, err)
	resp.Body.Close()

	resp, err = http.Get(admin.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "go_goroutines")
	require.Contains(t, string(body), "klaus_gateway_request_duration_seconds")
}
