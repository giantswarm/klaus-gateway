package a2aserver_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpoidc "github.com/giantswarm/mcp-oauth/providers/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/a2aserver"
)

const (
	testIssuer      = "https://dex.example.com"
	outsideIssuer   = "https://idp.example.org"
	clientAudience  = "klaus-gateway-app"
	outsideAudience = "klaus-gateway-outside"
	exchangeClient  = "klaus-gateway-exchange"
	applicationName = "example-app"
	outsideBackend  = "outside-backend"
)

// tokenSigner signs tokens of issuer and serves its key set over HTTP.
type tokenSigner struct {
	t       *testing.T
	issuer  string
	signer  jose.Signer
	jwksURL string
	jwks    *http.Client
}

func newTokenSigner(t *testing.T, issuer string) *tokenSigner {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	const kid = "test-key"
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), kid))
	require.NoError(t, err)
	set, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"}}})
	require.NoError(t, err)
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(set)
	}))
	t.Cleanup(jwks.Close)
	return &tokenSigner{t: t, issuer: issuer, signer: signer, jwksURL: jwks.URL, jwks: jwks.Client()}
}

func (s *tokenSigner) sign(claims map[string]any) string {
	s.t.Helper()
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = s.issuer
	}
	claims["sub"] = "subject"
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	claims["iat"] = time.Now().Unix()
	payload, err := json.Marshal(claims)
	require.NoError(s.t, err)
	signed, err := s.signer.Sign(payload)
	require.NoError(s.t, err)
	raw, err := signed.CompactSerialize()
	require.NoError(s.t, err)
	return raw
}

func (s *tokenSigner) trusted(name, audience, connector string, allowed ...string) a2aserver.TrustedIssuer {
	return a2aserver.TrustedIssuer{
		Name:    name,
		Issuer:  s.issuer,
		JWKSURL: s.jwksURL,
		// The test key set is served on loopback with a test certificate,
		// which the SSRF-safe default client refuses.
		JWKS:           mcpoidc.NewJWKSClientWithOptions(mcpoidc.JWKSClientOptions{HTTPClient: s.jwks, AllowPrivateIP: true}),
		Audience:       audience,
		AllowedClients: allowed,
		ConnectorID:    connector,
	}
}

func TestVerifierAcceptsAnApplicationToken(t *testing.T) {
	s := newTokenSigner(t, testIssuer)
	v := a2aserver.NewJWTVerifier([]a2aserver.TrustedIssuer{s.trusted("dex", clientAudience, "dex-self")}, exchangeClient)

	token := s.sign(map[string]any{"aud": []string{clientAudience, applicationName}, "azp": applicationName})
	verified, err := v.Verify(t.Context(), token)
	require.NoError(t, err)
	require.Equal(t, "dex-self", verified.Issuer.ConnectorID)
	require.False(t, verified.Expiry.IsZero(), "the token's exp is returned")

	sameClient := s.sign(map[string]any{"aud": clientAudience})
	_, err = v.Verify(t.Context(), sameClient)
	require.NoError(t, err, "a token without azp is issued to its only audience")
}

func TestVerifierRefusesTokensOfTheExchangeClient(t *testing.T) {
	s := newTokenSigner(t, testIssuer)
	v := a2aserver.NewJWTVerifier([]a2aserver.TrustedIssuer{s.trusted("dex", clientAudience, "dex-self")}, exchangeClient)

	exchanged := s.sign(map[string]any{"aud": []string{"kagent", clientAudience, exchangeClient}, "azp": exchangeClient})
	_, err := v.Verify(t.Context(), exchanged)
	require.ErrorContains(t, err, exchangeClient)

	v = a2aserver.NewJWTVerifier([]a2aserver.TrustedIssuer{s.trusted("dex", clientAudience, "dex-self")}, clientAudience)
	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": clientAudience}))
	require.Error(t, err, "without azp, the only audience is the requesting client")
}

func TestVerifierAllowsListedClientsOnly(t *testing.T) {
	s := newTokenSigner(t, testIssuer)
	v := a2aserver.NewJWTVerifier([]a2aserver.TrustedIssuer{s.trusted("dex", clientAudience, "dex-self", applicationName)}, exchangeClient)

	_, err := v.Verify(t.Context(), s.sign(map[string]any{"aud": []string{clientAudience, applicationName}, "azp": applicationName}))
	require.NoError(t, err)

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": []string{clientAudience, "other-app"}, "azp": "other-app"}))
	require.ErrorContains(t, err, "other-app")

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": []string{clientAudience, applicationName}}))
	require.Error(t, err, "a token naming no single client is not a listed client's")

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": []string{clientAudience, exchangeClient}, "azp": exchangeClient}))
	require.Error(t, err)
}

func TestVerifierChecksEachTokenAgainstItsOwnIssuer(t *testing.T) {
	dex := newTokenSigner(t, testIssuer)
	outside := newTokenSigner(t, outsideIssuer)
	v := a2aserver.NewJWTVerifier([]a2aserver.TrustedIssuer{
		dex.trusted("dex", clientAudience, "dex-self", applicationName),
		outside.trusted("outside", outsideAudience, "outside-idp", outsideBackend, "outside-backend-2"),
	}, exchangeClient)

	verified, err := v.Verify(t.Context(), dex.sign(map[string]any{"aud": clientAudience, "azp": applicationName}))
	require.NoError(t, err)
	require.Equal(t, "dex-self", verified.Issuer.ConnectorID)

	verified, err = v.Verify(t.Context(), outside.sign(map[string]any{"aud": outsideAudience, "azp": outsideBackend}))
	require.NoError(t, err)
	require.Equal(t, "outside-idp", verified.Issuer.ConnectorID)
	require.Equal(t, "outside", verified.Issuer.Name)

	for name, token := range map[string]string{
		"audience of the other issuer":     outside.sign(map[string]any{"aud": clientAudience, "azp": outsideBackend}),
		"client of the other issuer":       outside.sign(map[string]any{"aud": outsideAudience, "azp": applicationName}),
		"signed by the other issuer":       outside.sign(map[string]any{"iss": testIssuer, "aud": clientAudience, "azp": applicationName}),
		"unknown issuer":                   newTokenSigner(t, "https://other.example.net").sign(map[string]any{"aud": clientAudience, "azp": applicationName}),
		"expired":                          outside.sign(map[string]any{"aud": outsideAudience, "azp": outsideBackend, "exp": time.Now().Add(-time.Minute).Unix()}),
		"exchange client of any issuer":    outside.sign(map[string]any{"aud": outsideAudience, "azp": exchangeClient}),
		"not a token":                      "not-a-token",
		"issuer differing by a slash only": dex.sign(map[string]any{"iss": testIssuer + "/", "aud": clientAudience, "azp": applicationName}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(t.Context(), token)
			require.Error(t, err)
		})
	}
}

func TestVerifierReadsTheClientOfAnAccessToken(t *testing.T) {
	s := newTokenSigner(t, outsideIssuer)
	issuer := s.trusted("outside", outsideAudience, "outside-idp", outsideBackend)
	issuer.AccessTokens = true
	v := a2aserver.NewJWTVerifier([]a2aserver.TrustedIssuer{issuer}, exchangeClient)

	_, err := v.Verify(t.Context(), s.sign(map[string]any{"aud": []string{outsideAudience, "other-api"}, "client_id": outsideBackend}))
	require.NoError(t, err, "an RFC 9068 access token names its client in client_id")

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": outsideAudience, "client_id": "other-backend"}))
	require.ErrorContains(t, err, "other-backend")

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": outsideAudience, "client_id": exchangeClient}))
	require.ErrorContains(t, err, exchangeClient)

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": outsideAudience, "azp": "other-backend", "client_id": outsideBackend}))
	require.Error(t, err, "conflicting client identities are ambiguous")

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": outsideAudience, "azp": outsideBackend, "client_id": "denied"}))
	require.Error(t, err)

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": outsideAudience}))
	require.Error(t, err, "an access token's resource audience does not identify its client")

	_, err = v.Verify(t.Context(), s.sign(map[string]any{"aud": outsideAudience, "azp": outsideBackend}))
	require.NoError(t, err, "provider-specific access tokens may use azp alone")
}

func TestVerifierRequiresExactAudience(t *testing.T) {
	signer := newTokenSigner(t, testIssuer)
	verifier := a2aserver.NewJWTVerifier([]a2aserver.TrustedIssuer{signer.trusted("dex", clientAudience, "dex-self")}, exchangeClient)
	for _, audience := range []any{clientAudience + "/", []string{clientAudience + "/", applicationName}, strings.ToUpper(clientAudience)} {
		_, err := verifier.Verify(t.Context(), signer.sign(map[string]any{"aud": audience, "azp": applicationName}))
		require.Error(t, err)
	}
	_, err := verifier.Verify(t.Context(), signer.sign(map[string]any{"aud": []string{clientAudience + "/", clientAudience}, "azp": applicationName}))
	require.NoError(t, err)
}
