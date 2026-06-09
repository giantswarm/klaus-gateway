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

func TestValidate_A2ATargets(t *testing.T) {
	base := config.Defaults()
	base.A2A.Enabled = true
	base.A2A.CardURL = "http://gw/a2a"

	t.Run("static target sufficient", func(t *testing.T) {
		cfg := base
		cfg.A2A.StaticTarget = "http://pod:8080"
		require.NoError(t, cfg.Validate())
	})

	t.Run("multi-agent targets sufficient", func(t *testing.T) {
		cfg := base
		cfg.A2A.Targets = "worker-a=http://a:8080,worker-b=http://b:8080"
		require.NoError(t, cfg.Validate())
	})

	t.Run("targets takes precedence; static alone not required", func(t *testing.T) {
		cfg := base
		cfg.A2A.Targets = "worker-a=http://a:8080"
		cfg.A2A.StaticTarget = ""
		require.NoError(t, cfg.Validate())
	})

	t.Run("neither static nor targets fails", func(t *testing.T) {
		cfg := base
		require.Error(t, cfg.Validate())
	})
}

func TestA2ADefaults(t *testing.T) {
	cfg := config.Defaults()
	require.Equal(t, "klaus-worker", cfg.A2A.DefaultAgent)
}

func TestLoad_A2ATargetsEnv(t *testing.T) {
	t.Setenv("KLAUS_GATEWAY_A2A_TARGETS", "worker-a=http://a:8080,worker-b=http://b:8080")
	t.Setenv("KLAUS_GATEWAY_A2A_DEFAULT_AGENT", "worker-a")

	cfg, err := config.Load([]string{})
	require.NoError(t, err)
	require.Equal(t, "worker-a=http://a:8080,worker-b=http://b:8080", cfg.A2A.Targets)
	require.Equal(t, "worker-a", cfg.A2A.DefaultAgent)
}
