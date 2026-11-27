package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	cfg := config.A2AServerConfig{
		Enabled:          true,
		PublicURL:        "https://gateway.example.org",
		Issuers:          []config.CardIssuer{{Name: "dex", Issuer: "https://dex.example.org", Audience: "klaus-gateway-app"}},
		Token:            config.PlatformToken{Issuer: "https://dex.example.org", JWKSURL: "https://dex.example.org/keys", Audience: "klaus-gateway", AllowedClients: []string{"klaus-gateway-exchange"}},
		MaxBodyBytes:     config.DefaultA2AServerMaxBodyBytes,
		MinTokenLifetime: config.DefaultA2AServerMinTokenLifetime,
	}
	logger := slog.New(slog.DiscardHandler)
	public := chi.NewRouter()
	require.NoError(t, mountA2AServer(t.Context(), cfg, noAgents{}, noOrganizations{}, public, logger))
	handler := server.New(server.Options{Logger: logger, Public: public}).PublicHandler()

	for _, path := range []string{"/a2a", "/a2a/acme/billing"} {
		for name, authorization := range map[string]string{"no token": "", "malformed token": "Bearer not-a-jwt", "other scheme": "Basic dXNlcjpwYXNz"} { //nolint:gosec // G101: invalid credentials the gateway must refuse
			t.Run(path+" "+name, func(t *testing.T) {
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, unreadBody{t: t})
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
}
