package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/internal/config"
	"github.com/giantswarm/klaus-gateway/pkg/a2aserver"
	"github.com/giantswarm/klaus-gateway/pkg/server"
)

// unreadBody fails the test when anything reads it.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("the body of an unauthenticated request was read")
	return 0, io.EOF
}

type noOrganizations struct{}

func (noOrganizations) Organization(string) (string, string, bool) { return "", "", false }
func (noOrganizations) Shared(string) bool                         { return false }

// noAgents is never called: every request in these tests is refused first.
type noAgents struct{ a2aserver.Agents }

func TestPublicChainNeverReadsAnUnauthenticatedBody(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("s"), 0o600))
	cfg := config.A2AServerConfig{ //nolint:gosec // G101: test endpoints and a client id, no credential
		Enabled:                  true,
		PublicURL:                "https://gateway.example.org",
		Issuers:                  []config.TrustedIssuer{{Name: "dex", Issuer: "https://dex.example.org", JWKSURL: "https://dex.example.org/keys", Audience: "klaus-gateway-app", AllowedClients: []string{"app"}}},
		ExchangeTokenURL:         "https://dex.example.org/token",
		ExchangeClientID:         "klaus-gateway-exchange",
		ExchangeClientSecretFile: secret,
		ExchangeAudiences:        []string{"agent-platform"},
		MaxBodyBytes:             config.DefaultA2AServerMaxBodyBytes,
		MinTokenLifetime:         config.DefaultA2AServerMinTokenLifetime,
	}
	logger := slog.New(slog.DiscardHandler)
	public := chi.NewRouter()
	require.NoError(t, mountA2AServer(t.Context(), cfg, noAgents{}, noOrganizations{}, public, logger))
	handler := server.New(server.Options{Logger: logger, Public: public}).PublicHandler()

	for name, authorization := range map[string]string{"no token": "", "malformed token": "Bearer not-a-jwt", "other scheme": "Basic dXNlcjpwYXNz"} { //nolint:gosec // G101: invalid credentials the gateway must refuse
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/a2a", unreadBody{t: t})
			request.Header.Set("Content-Type", "application/json")
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusUnauthorized, recorder.Code)
		})
	}
}
