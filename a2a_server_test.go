package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/internal/config"
)

func TestTrustedIssuerRefusesAPrivateKeySetByDefault(t *testing.T) {
	for name, url := range map[string]string{
		"loopback": "https://127.0.0.1:1/keys",
		"private":  "https://10.0.0.1/keys",
		"plain":    "http://idp.example.org/keys",
	} {
		issuer, err := trustedIssuer(config.TrustedIssuer{Name: "x", Issuer: "https://idp.example.org", JWKSURL: url, Audience: "a"})
		require.NoError(t, err)
		_, err = issuer.JWKS.FetchJWKS(t.Context(), issuer.JWKSURL)
		require.Error(t, err, name)
	}
}
