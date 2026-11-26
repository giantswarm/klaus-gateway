package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestA2AExchangeRequiresHTTPS(t *testing.T) {
	for _, endpoint := range []string{"http://dex.example.com/token", "/token", "https:///token", "https://user:pass@dex.example.com/token", "https://dex.example.com/token#fragment"} {
		configuration := appBase()
		configuration.A2AServer.ExchangeTokenURL = endpoint
		require.Error(t, configuration.Validate(), endpoint)
	}
}

func TestA2ASingleIssuerPreservesIdentifier(t *testing.T) {
	configuration := appBase()
	configuration.A2AServer.Issuer = "https://idp.example.org/"
	require.NoError(t, configuration.Validate())
	issuer := configuration.A2AServer.TrustedIssuers()[0]
	require.Equal(t, configuration.A2AServer.Issuer, issuer.Issuer)
	require.Equal(t, "https://idp.example.org/keys", issuer.JWKSURL)
	require.Equal(t, "https://idp.example.org/token", issuer.TokenURL)
}
