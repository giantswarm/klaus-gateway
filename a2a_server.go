package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/go-chi/chi/v5"

	mcpoidc "github.com/giantswarm/mcp-oauth/providers/oidc"

	"github.com/giantswarm/klaus-gateway/internal/config"
	"github.com/giantswarm/klaus-gateway/internal/version"
	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"

	"github.com/giantswarm/klaus-gateway/pkg/a2aserver"
)

// a2aServerPath is where the A2A server is served.
const a2aServerPath = "/a2a"

// mountA2AServer serves the A2A server and its Agent Card.
func mountA2AServer(ctx context.Context, cfg config.A2AServerConfig, agents a2aserver.Agents, organizations pkga2a.OrganizationNames, r chi.Router, logger *slog.Logger) error {
	if organizations == nil {
		return fmt.Errorf("A2A clients address agents by organization: the gateway needs its Kubernetes client to read the organization namespaces")
	}
	secret, err := os.ReadFile(cfg.ExchangeClientSecretFile)
	if err != nil {
		return fmt.Errorf("read the exchange client secret: %w", err)
	}
	exchangeClient, err := issuerHTTPClient(cfg.CAFile)
	if err != nil {
		return err
	}
	issuers := make([]a2aserver.TrustedIssuer, 0, len(cfg.TrustedIssuers()))
	for _, entry := range cfg.TrustedIssuers() {
		issuer, err := trustedIssuer(entry)
		if err != nil {
			return fmt.Errorf("issuer %q: %w", entry.Name, err)
		}
		issuers = append(issuers, issuer)
	}
	card := a2aserver.NewCard(a2aserver.CardConfig{
		URL:     strings.TrimSuffix(cfg.PublicURL, "/") + a2aServerPath,
		Issuers: issuers,
		Version: version.Version(),
	})
	auth := &a2aserver.Authenticator{
		Verifier: a2aserver.NewJWTVerifier(issuers, cfg.ExchangeClientID),
		Exchanger: &a2aserver.Exchanger{
			TokenURL:     cfg.ExchangeEndpoint(),
			ClientID:     cfg.ExchangeClientID,
			ClientSecret: strings.TrimSpace(string(secret)),
			Audiences:    cfg.ExchangeAudiences,
			HTTPClient:   exchangeClient,
		},
		Realm:  "klaus-gateway",
		Logger: logger,
	}
	handler := a2aserver.NewRequestHandler(&a2aserver.Handler{Agents: agents, Card: card, Organizations: organizations, Logger: logger})
	r.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	r.Handle(a2aServerPath, a2aserver.LimitBody(cfg.MaxBodyBytes, auth.Middleware(a2aserver.EchoExtensions(a2asrv.NewJSONRPCHandler(handler)))))
	for _, issuer := range issuers {
		logger.Info("A2A server trusts issuer", "name", issuer.Name, "issuer", issuer.Issuer, "audience", issuer.Audience, "allowed_clients", issuer.AllowedClients, "connector_id", issuer.ConnectorID)
	}
	logger.Info("A2A server enabled", "endpoint", a2aServerPath, "exchange_token_url", cfg.ExchangeEndpoint(), "exchange_audiences", cfg.ExchangeAudiences, "max_body_bytes", cfg.MaxBodyBytes)
	return nil
}

// trustedIssuer is entry with its signing keys read from its key set URL,
// trusting its CA bundle next to the system roots.
func trustedIssuer(entry config.TrustedIssuer) (a2aserver.TrustedIssuer, error) {
	pool, err := issuerCertPool(entry.CAFile)
	if err != nil {
		return a2aserver.TrustedIssuer{}, err
	}
	discovery := entry.OpenIDConnectURL
	if discovery == "" && entry.TokenURL == "" {
		discovery = strings.TrimSuffix(entry.Issuer, "/") + "/.well-known/openid-configuration"
	}
	return a2aserver.TrustedIssuer{
		Name:    entry.Name,
		Issuer:  entry.Issuer,
		JWKSURL: entry.JWKSURL,
		JWKS: mcpoidc.NewJWKSClientWithOptions(mcpoidc.JWKSClientOptions{
			AllowPrivateIPHosts: entry.AllowPrivateIPJWKSHosts,
			RootCAs:             pool,
		}),
		Audience:         entry.Audience,
		AllowedClients:   entry.AllowedClients,
		ConnectorID:      entry.ConnectorID,
		TokenURL:         entry.TokenURL,
		MetadataURL:      entry.MetadataURL,
		OpenIDConnectURL: discovery,
		AccessTokens:     entry.SubjectTokenType == config.SubjectTokenAccessToken,
	}, nil
}

// issuerCertPool is the system roots with the PEM bundle at caFile, or nil
// (the system roots alone) when caFile is empty.
func issuerCertPool(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile) //nolint:gosec // G304: the CA bundle path is operator configuration (a chart value), not user input
	if err != nil {
		return nil, fmt.Errorf("read the issuer CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("issuer CA bundle %s holds no certificate", caFile)
	}
	return pool, nil
}

func issuerHTTPClient(caFile string) (*http.Client, error) {
	pool, err := issuerCertPool(caFile)
	if err != nil || pool == nil {
		return http.DefaultClient, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport}, nil
}

// propagatedExtensions are the A2A extensions the kagent client passes on from
// a request the A2A server serves: the MCP request metadata extension.
func propagatedExtensions(cfg config.A2AServerConfig) []string {
	if !cfg.Enabled {
		return nil
	}
	return []string{a2aserver.MCPRequestMetadataExtensionURI}
}
