package server_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/giantswarm/klaus-gateway/pkg/observability"
	"github.com/giantswarm/klaus-gateway/pkg/server"
)

// newAdmin constructs a server and returns just its admin handler wired into
// an httptest.Server.
func newAdmin(t *testing.T, ready server.ReadinessFunc) *httptest.Server {
	t.Helper()
	s := server.New(server.Options{
		PublicAddress: "127.0.0.1:0",
		AdminAddress:  "127.0.0.1:0",
		Metrics:       observability.NewMetrics(),
		Ready:         ready,
	})
	ts := httptest.NewServer(s.AdminHandler())
	t.Cleanup(ts.Close)
	return ts
}

func TestAdmin_Healthz(t *testing.T) {
	ts := newAdmin(t, nil)
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAdmin_Readyz_OK(t *testing.T) {
	ts := newAdmin(t, func(context.Context) error { return nil })
	resp, err := http.Get(ts.URL + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAdmin_Readyz_NotReady(t *testing.T) {
	ts := newAdmin(t, func(context.Context) error { return errors.New("store down") })
	resp, err := http.Get(ts.URL + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestAdmin_Metrics(t *testing.T) {
	srv := server.New(server.Options{
		PublicAddress: "127.0.0.1:0",
		AdminAddress:  "127.0.0.1:0",
		Metrics:       observability.NewMetrics(),
	})
	public := httptest.NewServer(srv.PublicHandler())
	t.Cleanup(public.Close)
	admin := httptest.NewServer(srv.AdminHandler())
	t.Cleanup(admin.Close)

	// Drive one public request so the RED histograms receive a sample.
	resp, err := http.Get(public.URL + "/warmup")
	require.NoError(t, err)
	_ = resp.Body.Close()

	resp, err = http.Get(admin.URL + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "go_goroutines")
	require.Contains(t, string(body), "klaus_gateway_request_duration_seconds")
	require.Contains(t, string(body), "klaus_gateway_requests_total")
}

func TestPublic_EmitsOTelSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	s := server.New(server.Options{
		PublicAddress: "127.0.0.1:0",
		AdminAddress:  "127.0.0.1:0",
		Metrics:       observability.NewMetrics(),
	})
	ts := httptest.NewServer(s.PublicHandler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/nope")
	require.NoError(t, err)
	_ = resp.Body.Close()

	// Give the batcher a moment to flush.
	require.Eventually(t, func() bool {
		return len(recorder.Ended()) > 0
	}, 2*time.Second, 10*time.Millisecond)
}
