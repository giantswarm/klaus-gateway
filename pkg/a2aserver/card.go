package a2aserver

import (
	"github.com/a2aproject/a2a-go/v2/a2a"
)

// ScopeOpenID is the scope an A2A client requests its token with.
const ScopeOpenID = "openid"

// CardConfig describes the public Agent Card.
type CardConfig struct {
	// URL is the public URL of the JSON-RPC endpoint.
	URL string
	// Issuers are the identity providers an A2A client may get its token
	// from; each is one security scheme of the card.
	Issuers []TrustedIssuer
	// Version is the gateway's version.
	Version string
	// ToolCallMetadataKeys are the metadata keys the MCP tool-call metadata
	// extension accepts; empty does not declare the extension.
	ToolCallMetadataKeys []string
}

// NewCard returns the public Agent Card: the endpoint, one security scheme
// per trusted issuer, any one of which is enough, and an extended card
// naming the agents per caller.
func NewCard(cfg CardConfig) *a2a.AgentCard {
	schemes := make(a2a.NamedSecuritySchemes, len(cfg.Issuers))
	requirements := make(a2a.SecurityRequirementsOptions, 0, len(cfg.Issuers))
	for _, issuer := range cfg.Issuers {
		name := a2a.SecuritySchemeName(issuer.Name)
		schemes[name] = securityScheme(issuer)
		requirements = append(requirements, a2a.SecurityRequirements{name: {ScopeOpenID}})
	}
	capabilities := a2a.AgentCapabilities{Streaming: true, ExtendedAgentCard: true}
	if len(cfg.ToolCallMetadataKeys) > 0 {
		capabilities.Extensions = []a2a.AgentExtension{toolCallMetadataCardExtension(cfg.ToolCallMetadataKeys)}
	}
	return &a2a.AgentCard{
		Name:        "Agent Platform",
		Description: "Agents of the platform. The authenticated extended card lists the agents the caller may use, one interface per agent, its tenant naming the agent.",
		Version:     cfg.Version,
		SupportedInterfaces: []*a2a.AgentInterface{{
			URL:             cfg.URL,
			ProtocolBinding: a2a.TransportProtocolJSONRPC,
			ProtocolVersion: a2a.Version,
		}},
		Capabilities:         capabilities,
		DefaultInputModes:    []string{"text/plain"},
		DefaultOutputModes:   []string{"text/plain"},
		Skills:               []a2a.AgentSkill{},
		SecuritySchemes:      schemes,
		SecurityRequirements: requirements,
	}
}

// securityScheme is an OAuth 2.0 client credentials scheme at the issuer's
// token endpoint, naming its RFC 8414 metadata when known, or, for an issuer
// without a token endpoint, an OpenID Connect scheme at its discovery
// document.
func securityScheme(issuer TrustedIssuer) a2a.SecurityScheme {
	description := "A token issued by " + issuer.Issuer + " for the audience " + issuer.Audience + "."
	if issuer.TokenURL == "" {
		return a2a.OpenIDConnectSecurityScheme{Description: description, OpenIDConnectURL: issuer.OpenIDConnectURL}
	}
	return a2a.OAuth2SecurityScheme{
		Description:       description,
		Flows:             a2a.ClientCredentialsOAuthFlow{TokenURL: issuer.TokenURL, Scopes: map[string]string{ScopeOpenID: "OpenID Connect"}},
		Oauth2MetadataURL: issuer.MetadataURL,
	}
}
