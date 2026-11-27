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
	testIssuer       = "https://dex.example.com"
	outsideIssuer    = "https://idp.example.org"
	platformAudience = "klaus-gateway"
	clientAudience   = "klaus-gateway-app"
	outsideAudience  = "klaus-gateway-outside"
	exchangeClient   = "klaus-gateway-exchange"
	applicationName  = "example-app"
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

// verifier accepts platform tokens of s for platformAudience requested by
// allowed.
func (s *tokenSigner) verifier(allowed ...string) *a2aserver.PlatformVerifier {
	return &a2aserver.PlatformVerifier{
		Issuer:  s.issuer,
		JWKSURL: s.jwksURL,
		// The test key set is served on loopback with a test certificate,
		// which the SSRF-safe default client refuses.
		JWKS:           mcpoidc.NewJWKSClientWithOptions(mcpoidc.JWKSClientOptions{HTTPClient: s.jwks, AllowPrivateIP: true}),
		Audience:       platformAudience,
		AllowedClients: allowed,
	}
}

func TestPlatformVerifierAcceptsTheExchangedToken(t *testing.T) {
	dex := newTokenSigner(t, testIssuer)
	v := dex.verifier(exchangeClient)

	verified, err := v.Verify(t.Context(), dex.sign(map[string]any{"aud": []string{"kagent", platformAudience, exchangeClient}, "azp": exchangeClient}))
	require.NoError(t, err)
	require.False(t, verified.Expiry.IsZero(), "the token's exp is returned")

	_, err = v.Verify(t.Context(), dex.sign(map[string]any{"aud": platformAudience, "azp": exchangeClient}))
	require.NoError(t, err)
}

func TestPlatformVerifierRefusesOtherTokens(t *testing.T) {
	dex := newTokenSigner(t, testIssuer)
	outside := newTokenSigner(t, outsideIssuer)
	v := dex.verifier(exchangeClient)

	for name, token := range map[string]string{
		"another client":                      dex.sign(map[string]any{"aud": []string{platformAudience, applicationName}, "azp": applicationName}),
		"another audience":                    dex.sign(map[string]any{"aud": []string{clientAudience, exchangeClient}, "azp": exchangeClient}),
		"audience differing by a slash":       dex.sign(map[string]any{"aud": platformAudience + "/", "azp": exchangeClient}),
		"audience differing by case":          dex.sign(map[string]any{"aud": strings.ToUpper(platformAudience), "azp": exchangeClient}),
		"multiple audiences without azp":      dex.sign(map[string]any{"aud": []string{platformAudience, exchangeClient}}),
		"sole audience not an allowed client": dex.sign(map[string]any{"aud": platformAudience}),
		"outside token sent directly":         outside.sign(map[string]any{"aud": outsideAudience, "azp": "outside-backend"}),
		"outside token for the audience":      outside.sign(map[string]any{"aud": platformAudience, "azp": exchangeClient}),
		"outside key, platform issuer":        outside.sign(map[string]any{"iss": testIssuer, "aud": platformAudience, "azp": exchangeClient}),
		"expired":                             dex.sign(map[string]any{"aud": platformAudience, "azp": exchangeClient, "exp": time.Now().Add(-time.Minute).Unix()}),
		"issuer differing by a slash":         dex.sign(map[string]any{"iss": testIssuer + "/", "aud": platformAudience, "azp": exchangeClient}),
		"not a token":                         "not-a-token",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(t.Context(), token)
			require.Error(t, err)
		})
	}
}

func TestPlatformVerifierAcceptsTheSoleAudienceAsClient(t *testing.T) {
	dex := newTokenSigner(t, testIssuer)
	v := dex.verifier(platformAudience)
	_, err := v.Verify(t.Context(), dex.sign(map[string]any{"aud": platformAudience}))
	require.NoError(t, err, "a token without azp is issued to its only audience")
}
