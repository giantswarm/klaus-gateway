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
