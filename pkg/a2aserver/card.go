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
	Issuers []CardIssuer
	// Version is the gateway's version.
	Version string
}

// CardIssuer is an identity provider an A2A client may get its token from,
// declared as one security scheme of the Agent Card. agentgateway checks the
// tokens; the gateway only names the issuer.
type CardIssuer struct {
	// Name names the issuer's security scheme.
	Name string
	// Issuer is the issuer's identifier.
	Issuer string
	// Audience is the client a token must be issued for.
	Audience string
	// TokenURL, when set, makes the scheme OAuth 2.0 client credentials at
	// this token endpoint.
	TokenURL string
	// MetadataURL is the RFC 8414 metadata the OAuth 2.0 scheme names, if
	// any.
	MetadataURL string
	// OpenIDConnectURL is the discovery document the OpenID Connect scheme
	// names when TokenURL is empty.
	OpenIDConnectURL string
}

// NewCard returns the public routing endpoint and one security scheme per
// trusted issuer, any one of which is enough. Agents have their own cards.
func NewCard(configuration CardConfig) *a2a.AgentCard {
	schemes := make(a2a.NamedSecuritySchemes, len(configuration.Issuers))
	requirements := make(a2a.SecurityRequirementsOptions, 0, len(configuration.Issuers))
	for _, issuer := range configuration.Issuers {
		name := a2a.SecuritySchemeName(issuer.Name)
		schemes[name] = securityScheme(issuer)
		requirements = append(requirements, a2a.SecurityRequirements{name: {ScopeOpenID}})
	}
	capabilities := a2a.AgentCapabilities{Streaming: true}
	return &a2a.AgentCard{
		Name:        "Agent Platform",
		Description: "Agents of the platform, addressed by tenant. Configure the Agent Card URL of the agent to use.",
		Version:     configuration.Version,
		SupportedInterfaces: []*a2a.AgentInterface{{
			URL:             configuration.URL,
			ProtocolBinding: a2a.TransportProtocolJSONRPC,
			ProtocolVersion: a2a.Version,
		}},
		Capabilities:       capabilities,
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{
			ID: "agent", Name: "Run an agent", Description: "Run the agent selected by the request's tenant.", Tags: []string{"agent"},
		}},
		SecuritySchemes:      schemes,
		SecurityRequirements: requirements,
	}
}

// securityScheme is an OAuth 2.0 client credentials scheme at the issuer's
// token endpoint, naming its RFC 8414 metadata when known, or, for an issuer
// without a token endpoint, an OpenID Connect scheme at its discovery
// document.
func securityScheme(issuer CardIssuer) a2a.SecurityScheme {
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
