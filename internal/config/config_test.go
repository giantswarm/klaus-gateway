package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/internal/config"
)

func TestLoad_EnvAndFlagPrecedence(t *testing.T) {
	t.Setenv("KLAUS_GATEWAY_LOG_LEVEL", "debug")
	t.Setenv("KLAUS_GATEWAY_STORE", "memory")

	cfg, err := config.Load([]string{"--store", "bolt", "--bolt-path", "/tmp/r"})
	require.NoError(t, err)
	require.Equal(t, "bolt", cfg.Store, "flag overrides env")
	require.Equal(t, "debug", cfg.LogLevel, "env applies when no flag given")
	require.Equal(t, "/tmp/r", cfg.BoltPath)
}

func TestValidate(t *testing.T) {
	cfg := config.Defaults()
	require.NoError(t, cfg.Validate())

	bad := cfg
	bad.Store = "redis"
	require.Error(t, bad.Validate())

	badBolt := cfg
	badBolt.Store = "bolt"
	badBolt.BoltPath = ""
	require.Error(t, badBolt.Validate())

	badOp := cfg
	badOp.Driver = "operator"
	badOp.OperatorMCPURL = ""
	require.Error(t, badOp.Validate())

	staticEmpty := cfg
	staticEmpty.Driver = "static"
	staticEmpty.StaticInstances = ""
	require.NoError(t, staticEmpty.Validate(), "static driver with no instances is valid (A2A-only deployments)")
}

func TestValidate_A2A(t *testing.T) {
	base := config.Defaults()
	base.A2A.Enabled = true
	base.A2A.URL = "http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a/kagent"

	t.Run("enabled with url is valid", func(t *testing.T) {
		require.NoError(t, base.Validate())
	})

	t.Run("enabled without url fails", func(t *testing.T) {
		cfg := base
		cfg.A2A.URL = ""
		require.Error(t, cfg.Validate())
	})
}

func TestValidate_OBO(t *testing.T) {
	base := config.Defaults()
	base.OBO = config.OBOConfig{
		Enabled:         true,
		MusterURL:       "https://muster.example.com",
		CallbackBaseURL: "https://gateway.example.com",
		StateKeyFile:    "/etc/obo/state.key",
	}

	t.Run("enabled with required fields is valid", func(t *testing.T) {
		require.NoError(t, base.Validate())
	})

	t.Run("missing muster url fails", func(t *testing.T) {
		cfg := base
		cfg.OBO.MusterURL = ""
		require.Error(t, cfg.Validate())
	})

	t.Run("client id is optional (derived from CIMD url)", func(t *testing.T) {
		cfg := base
		cfg.OBO.ClientID = ""
		require.NoError(t, cfg.Validate(), "client_id defaults to the self-hosted CIMD document URL")
	})

	t.Run("missing callback base url fails", func(t *testing.T) {
		cfg := base
		cfg.OBO.CallbackBaseURL = ""
		require.Error(t, cfg.Validate())
	})

	t.Run("missing state key file fails", func(t *testing.T) {
		cfg := base
		cfg.OBO.StateKeyFile = ""
		require.Error(t, cfg.Validate())
	})

	t.Run("store path without key fails", func(t *testing.T) {
		cfg := base
		cfg.OBO.StorePath = "/var/lib/obo/links.bolt"
		require.Error(t, cfg.Validate())
	})

	t.Run("store path with key is valid", func(t *testing.T) {
		cfg := base
		cfg.OBO.StorePath = "/var/lib/obo/links.bolt"
		cfg.OBO.StoreKeyFile = "/etc/obo/store.key"
		require.NoError(t, cfg.Validate())
	})

	t.Run("disabled skips all obo checks", func(t *testing.T) {
		cfg := config.Defaults()
		require.NoError(t, cfg.Validate())
	})
}

func TestLoad_OBOEnv(t *testing.T) {
	t.Setenv("KLAUS_GATEWAY_OBO_ENABLED", "true")
	t.Setenv("KLAUS_GATEWAY_OBO_MUSTER_URL", "https://muster.example.com")
	t.Setenv("KLAUS_GATEWAY_OBO_CLIENT_ID", "klaus-gateway")

	cfg, err := config.Load([]string{"--obo-callback-base-url", "https://gateway.example.com"})
	require.NoError(t, err)
	require.True(t, cfg.OBO.Enabled)
	require.Equal(t, "https://muster.example.com", cfg.OBO.MusterURL)
	require.Equal(t, "klaus-gateway", cfg.OBO.ClientID)
	require.Equal(t, "https://gateway.example.com", cfg.OBO.CallbackBaseURL, "flag sets callback base url")
}

func TestA2ADefaults(t *testing.T) {
	cfg := config.Defaults()
	require.Equal(t, "klaud-coding", cfg.A2A.DefaultAgent)
}

func TestLoad_A2ADefaultAgentEnv(t *testing.T) {
	t.Setenv("KLAUS_GATEWAY_A2A_DEFAULT_AGENT", "worker-a")

	cfg, err := config.Load([]string{})
	require.NoError(t, err)
	require.Equal(t, "worker-a", cfg.A2A.DefaultAgent)
}
