package a2aserver_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/a2aserver"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestExchangeRejectsInsecureEndpointsBeforeSendingCredentials(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)
	for _, endpoint := range []string{server.URL, "/token", "https:///token", "https://user:password@example.com/token", "https://example.com/token#fragment"} {
		exchanger := &a2aserver.Exchanger{TokenURL: endpoint, ClientID: exchangeClient, ClientSecret: "test-secret"}
		_, err := exchanger.Exchange(t.Context(), clientToken, a2aserver.TrustedIssuer{})
		require.Error(t, err)
	}
	require.Zero(t, calls.Load())
}

func TestExchangeNeverFollowsRedirects(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	t.Cleanup(destination.Close)
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				http.Redirect(response, request, destination.URL, status)
			}))
			t.Cleanup(server.Close)
			exchanger := &a2aserver.Exchanger{TokenURL: server.URL, ClientID: exchangeClient, ClientSecret: "test-secret", HTTPClient: server.Client()}
			_, err := exchanger.Exchange(t.Context(), clientToken, a2aserver.TrustedIssuer{})
			require.Error(t, err)
			require.Nil(t, exchanger.HTTPClient.CheckRedirect, "the shared client is not mutated")
		})
	}
	require.Zero(t, redirected.Load())
}

func TestExchangeRequiresBearerUsageForDexProfile(t *testing.T) {
	for _, usage := range []string{"", "N_A", "DPoP", "Bearer", "bearer", "bEaReR"} {
		t.Run(usage, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(response, `{"access_token":"test-issued","issued_token_type":"urn:ietf:params:oauth:token-type:id_token","token_type":%q}`, usage)
			}))
			t.Cleanup(server.Close)
			exchanger := &a2aserver.Exchanger{TokenURL: server.URL, HTTPClient: server.Client()}
			_, err := exchanger.Exchange(t.Context(), clientToken, a2aserver.TrustedIssuer{})
			if usage == "Bearer" || usage == "bearer" || usage == "bEaReR" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestExchangeHasIndependentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		//nolint:gosec // G101: the token endpoint is a test URL, not a credential.
		exchanger := &a2aserver.Exchanger{TokenURL: "https://dex.example.com/token", HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})}}
		_, err := exchanger.Exchange(t.Context(), clientToken, a2aserver.TrustedIssuer{})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 10*time.Second, time.Since(started))
	})
}
