package main

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
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
func mountA2AServer(_ context.Context, cfg config.A2AServerConfig, agents a2aserver.Agents, organizations pkga2a.OrganizationNames, r chi.Router, logger *slog.Logger) error {
	if organizations == nil {
		return fmt.Errorf("A2A clients address agents by organization: the gateway needs its Kubernetes client to read the organization namespaces")
	}
	verifier, err := platformVerifier(cfg.Token, cfg.CAFile)
	if err != nil {
		return err
	}
	issuers := make([]a2aserver.CardIssuer, 0, len(cfg.Issuers))
	for _, entry := range cfg.Issuers {
		issuers = append(issuers, cardIssuer(entry))
	}
	card := a2aserver.NewCard(a2aserver.CardConfig{
		URL:     strings.TrimSuffix(cfg.PublicURL, "/") + a2aServerPath,
		Issuers: issuers,
		Version: version.Version(),
	})
	auth := &a2aserver.Authenticator{
		Verifier:         verifier,
		MaxBodyBytes:     cfg.MaxBodyBytes,
		MinTokenLifetime: cfg.MinTokenLifetime,
		Realm:            "klaus-gateway",
		Logger:           logger,
	}
	handler := a2aserver.NewRequestHandler(&a2aserver.Handler{Agents: agents, Card: card, Organizations: organizations, Logger: logger})
	r.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	r.Handle(a2aServerPath, auth.Middleware(a2asrv.NewJSONRPCHandler(handler)))
	for _, issuer := range issuers {
		logger.Info("A2A server card declares issuer", "name", issuer.Name, "issuer", issuer.Issuer, "audience", issuer.Audience)
	}
	logger.Info("A2A server enabled", "endpoint", a2aServerPath, "token_issuer", cfg.Token.Issuer, "token_audience", cfg.Token.Audience, "token_allowed_clients", cfg.Token.AllowedClients, "max_body_bytes", cfg.MaxBodyBytes, "min_token_lifetime", cfg.MinTokenLifetime)
	return nil
}

// platformVerifier verifies the platform token with token's key set, trusting
// the CA bundle at caFile next to the system roots.
func platformVerifier(token config.PlatformToken, caFile string) (*a2aserver.PlatformVerifier, error) {
	pool, err := issuerCertPool(caFile)
	if err != nil {
		return nil, err
	}
	return &a2aserver.PlatformVerifier{
		Issuer:  token.Issuer,
		JWKSURL: token.JWKSURL,
		JWKS: mcpoidc.NewJWKSClientWithOptions(mcpoidc.JWKSClientOptions{
			AllowPrivateIPHosts: token.AllowPrivateIPJWKSHosts,
			RootCAs:             pool,
		}),
		Audience:       token.Audience,
		AllowedClients: token.AllowedClients,
	}, nil
}

// cardIssuer is entry as the Agent Card declares it. An issuer without a token
// endpoint or discovery document names its default discovery document.
func cardIssuer(entry config.CardIssuer) a2aserver.CardIssuer {
	discovery := entry.OpenIDConnectURL
	if discovery == "" && entry.TokenURL == "" {
		discovery = strings.TrimSuffix(entry.Issuer, "/") + "/.well-known/openid-configuration"
	}
	return a2aserver.CardIssuer{
		Name:             entry.Name,
		Issuer:           entry.Issuer,
		Audience:         entry.Audience,
		TokenURL:         entry.TokenURL,
		MetadataURL:      entry.MetadataURL,
		OpenIDConnectURL: discovery,
	}
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
