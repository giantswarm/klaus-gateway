package a2aserver

import (
	"context"
	"fmt"
	"slices"

	mcpoidc "github.com/giantswarm/mcp-oauth/providers/oidc"
)

// PlatformVerifier verifies the platform token agentgateway sends after it
// has checked an A2A client's token and exchanged it at the installation's
// identity provider: an ID token of Issuer for Audience, requested by one of
// AllowedClients.
type PlatformVerifier struct {
	// Issuer is the token's iss, exactly.
	Issuer string
	// JWKSURL is the issuer's key set.
	JWKSURL string
	// JWKS fetches and caches the key set (mcp-oauth's SSRF-safe client).
	JWKS *mcpoidc.JWKSClient
	// Audience is the client the token must be issued for, exactly.
	Audience string
	// AllowedClients are the only clients whose tokens are accepted: the
	// token's azp, or its only aud.
	AllowedClients []string
}

// Verify checks the token with mcp-oauth's ValidateIDToken (signature with a
// pinned asymmetric algorithm, expiry, not-before, issuer and audience), then
// the client that requested it.
func (v *PlatformVerifier) Verify(ctx context.Context, rawToken string) (VerifiedToken, error) {
	claims, err := mcpoidc.ValidateIDToken(ctx, rawToken, v.JWKS, v.JWKSURL, v.Issuer, []string{v.Audience})
	if err != nil {
		return VerifiedToken{}, err
	}
	audience := []string(claims.Audience)
	if !slices.Contains(audience, v.Audience) {
		return VerifiedToken{}, fmt.Errorf("token audience does not match %q exactly", v.Audience)
	}
	client := claims.AuthorizedParty
	if client == "" {
		if len(audience) != 1 {
			return VerifiedToken{}, fmt.Errorf("ID token with multiple audiences must identify its authorized party")
		}
		client = audience[0]
	}
	if !slices.Contains(v.AllowedClients, client) {
		return VerifiedToken{}, fmt.Errorf("token requested by client %q, which is not allowed", client)
	}
	var verified VerifiedToken
	if claims.Expiry != nil {
		verified.Expiry = claims.Expiry.Time()
	}
	return verified, nil
}
