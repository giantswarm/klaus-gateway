package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/internal/config"
)

func TestPlatformVerifierRefusesAPrivateKeySetByDefault(t *testing.T) {
	for name, url := range map[string]string{
		"loopback": "https://127.0.0.1:1/keys",
		"private":  "https://10.0.0.1/keys",
		"plain":    "http://idp.example.org/keys",
	} {
		verifier, err := platformVerifier(config.PlatformToken{Issuer: "https://dex.example.com", JWKSURL: url, Audience: "a", AllowedClients: []string{"c"}}, "")
		require.NoError(t, err)
		_, err = verifier.JWKS.FetchJWKS(t.Context(), verifier.JWKSURL)
		require.Error(t, err, name)
	}
}
