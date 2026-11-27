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
