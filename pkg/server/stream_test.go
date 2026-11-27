package server_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/observability"
	"github.com/giantswarm/klaus-gateway/pkg/server"
)

func TestPublicStreamFlushesBeforeHandlerReturns(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		protocol := "HTTP1"
		if http2 {
			protocol = "HTTP2"
		}
		t.Run(protocol, func(t *testing.T) {
			released := make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			application := server.New(server.Options{Public: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				flusher, ok := response.(http.Flusher)
				if !ok {
					http.Error(response, "streaming unavailable", http.StatusInternalServerError)
					return
				}
				response.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(response, "data: first\n\n")
				flusher.Flush()
				select {
				case <-released:
					_, _ = io.WriteString(response, "data: last\n\n")
				case <-request.Context().Done():
				}
			})})
			endpoint := httptest.NewUnstartedServer(application.PublicHandler())
			endpoint.EnableHTTP2 = http2
			endpoint.StartTLS()
			t.Cleanup(endpoint.Close)
			t.Cleanup(release)
			caller := endpoint.Client()
			caller.Timeout = 5 * time.Second
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint.URL, nil)
			require.NoError(t, err)
			response, err := caller.Do(request)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
			if http2 {
				require.Equal(t, 2, response.ProtoMajor)
			} else {
				require.Equal(t, 1, response.ProtoMajor)
			}
			reader := bufio.NewReader(response.Body)
			first, err := reader.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "data: first\n", first)
			release()
			remainder, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Equal(t, "\ndata: last\n\n", string(remainder))
		})
	}
}

func TestPublicObservabilityRecordsFinalStatus(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		code       int
		body       string
		wantStatus int
	}{
		{name: "empty response", wantStatus: http.StatusOK},
		{name: "implicit success", body: "ok", wantStatus: http.StatusOK},
		{name: "explicit status", code: http.StatusTeapot, wantStatus: http.StatusTeapot},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			metrics := observability.NewMetrics()
			var logs bytes.Buffer
			application := server.New(server.Options{
				Metrics: metrics,
				Logger:  slog.New(slog.NewJSONHandler(&logs, nil)),
				Public: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
					if scenario.code != 0 {
						response.WriteHeader(scenario.code)
					}
					if scenario.body != "" {
						_, _ = io.WriteString(response, scenario.body)
					}
				}),
			})
			response := httptest.NewRecorder()
			application.PublicHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(t.Context()))
			require.Equal(t, scenario.wantStatus, response.Code)
			require.Equal(t, float64(1), testutil.ToFloat64(metrics.RequestsTotal.WithLabelValues("public", http.MethodGet, strconv.Itoa(scenario.wantStatus))))
			var event map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &event))
			require.Equal(t, float64(scenario.wantStatus), event["status"])
		})
	}
}
