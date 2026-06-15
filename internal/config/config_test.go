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
