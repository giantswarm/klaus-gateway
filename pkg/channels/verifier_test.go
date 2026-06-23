package channels_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

type idClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Expiry   int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
}

// newTestIssuer spins an httptest OIDC issuer serving a discovery document and
// JWKS, and returns its issuer URL plus a function that mints signed JWTs.
func newTestIssuer(t *testing.T) (string, func(idClaims) string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	const kid = "test-key"

	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"jwks_uri":                              issuer + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       priv.Public(),
			KeyID:     kid,
			Algorithm: "RS256",
			Use:       "sig",
		}}})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	issuer = ts.URL

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	require.NoError(t, err)

	mint := func(c idClaims) string {
		payload, err := json.Marshal(c)
		require.NoError(t, err)
		obj, err := signer.Sign(payload)
		require.NoError(t, err)
		token, err := obj.CompactSerialize()
		require.NoError(t, err)
		return token
	}
	return issuer, mint
}

func TestOIDCTokenVerifier_Valid(t *testing.T) {
	issuer, mint := newTestIssuer(t)
	now := time.Now()
	token := mint(idClaims{
		Issuer:   issuer,
		Subject:  "user-1",
		Audience: "klaus-gateway",
		IssuedAt: now.Unix(),
		Expiry:   now.Add(time.Hour).Unix(),
	})

	v := channels.NewOIDCTokenVerifier(channels.OIDCVerifierConfig{Issuer: issuer, Audience: "klaus-gateway"})
	sub, err := v.Verify(t.Context(), token)
	require.NoError(t, err)
	require.Equal(t, "user-1", sub)
}

func TestOIDCTokenVerifier_WrongIssuer(t *testing.T) {
	issuer, mint := newTestIssuer(t)
	now := time.Now()
	token := mint(idClaims{
		Issuer:   "https://attacker.example.com",
		Subject:  "user-1",
		Audience: "klaus-gateway",
		IssuedAt: now.Unix(),
		Expiry:   now.Add(time.Hour).Unix(),
	})

	v := channels.NewOIDCTokenVerifier(channels.OIDCVerifierConfig{Issuer: issuer, Audience: "klaus-gateway"})
	_, err := v.Verify(t.Context(), token)
	require.Error(t, err)
}

func TestOIDCTokenVerifier_WrongAudience(t *testing.T) {
	issuer, mint := newTestIssuer(t)
	now := time.Now()
	token := mint(idClaims{
		Issuer:   issuer,
		Subject:  "user-1",
		Audience: "some-other-service",
		IssuedAt: now.Unix(),
		Expiry:   now.Add(time.Hour).Unix(),
	})

	v := channels.NewOIDCTokenVerifier(channels.OIDCVerifierConfig{Issuer: issuer, Audience: "klaus-gateway"})
	_, err := v.Verify(t.Context(), token)
	require.Error(t, err)
}

func TestOIDCTokenVerifier_Expired(t *testing.T) {
	issuer, mint := newTestIssuer(t)
	now := time.Now()
	token := mint(idClaims{
		Issuer:   issuer,
		Subject:  "user-1",
		Audience: "klaus-gateway",
		IssuedAt: now.Add(-2 * time.Hour).Unix(),
		Expiry:   now.Add(-time.Hour).Unix(),
	})

	v := channels.NewOIDCTokenVerifier(channels.OIDCVerifierConfig{Issuer: issuer, Audience: "klaus-gateway"})
	_, err := v.Verify(t.Context(), token)
	require.Error(t, err)
}

func TestOIDCTokenVerifier_EmptyToken(t *testing.T) {
	issuer, _ := newTestIssuer(t)
	v := channels.NewOIDCTokenVerifier(channels.OIDCVerifierConfig{Issuer: issuer, Audience: "klaus-gateway"})
	_, err := v.Verify(t.Context(), "")
	require.Error(t, err)
}
