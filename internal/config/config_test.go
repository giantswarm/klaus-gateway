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

	t.Run("fallback icon template with placeholder is valid", func(t *testing.T) {
		cfg := base
		cfg.A2A.FallbackIconURLTemplate = "https://avatars.gazelle.awsprod.gigantic.io/v1/{agent}.png"
		require.NoError(t, cfg.Validate())
	})

	t.Run("fallback icon template without placeholder fails", func(t *testing.T) {
		cfg := base
		cfg.A2A.FallbackIconURLTemplate = "https://avatars.gazelle.awsprod.gigantic.io/v1/agent.png"
		require.Error(t, cfg.Validate())
	})
}

func TestLoad_A2AFallbackIconURLTemplateEnv(t *testing.T) {
	t.Setenv("KLAUS_GATEWAY_A2A_FALLBACK_ICON_URL_TEMPLATE", "https://avatars.gazelle.awsprod.gigantic.io/v1/{agent}.png")

	cfg, err := config.Load([]string{"--a2a-fallback-icon-url-template=https://cli.example/{agent}.svg"})
	require.NoError(t, err)
	require.Equal(t, "https://cli.example/{agent}.svg", cfg.A2A.FallbackIconURLTemplate, "the flag overrides the env value")
}

func TestLoad_WebEnabledEnv(t *testing.T) {
	t.Setenv("KLAUS_GATEWAY_WEB_ENABLED", "false")

	cfg, err := config.Load(nil)
	require.NoError(t, err)
	require.False(t, cfg.Web.Enabled)
}

func TestWebEnabledByDefault(t *testing.T) {
	cfg, err := config.Load(nil)
	require.NoError(t, err)
	require.True(t, cfg.Web.Enabled, "the web adapter stays on by default for local development")
}

func TestValidate_SlackSurfaces(t *testing.T) {
	base := config.Defaults()
	base.Slack.Enabled = true

	t.Run("defaults are valid", func(t *testing.T) {
		require.NoError(t, base.Validate())
	})

	t.Run("unknown dm mode fails", func(t *testing.T) {
		cfg := base
		cfg.Slack.DMMode = "sometimes"
		require.Error(t, cfg.Validate())
	})

	t.Run("unknown channel mode fails", func(t *testing.T) {
		cfg := base
		cfg.Slack.ChannelMode = "most"
		require.Error(t, cfg.Validate())
	})

	t.Run("allowlist mode requires a non-empty list", func(t *testing.T) {
		cfg := base
		cfg.Slack.ChannelMode = "allowlist"
		require.Error(t, cfg.Validate())
		cfg.Slack.ChannelAllowlist = []string{"C1"}
		require.NoError(t, cfg.Validate())
	})

	t.Run("allowlist set without allowlist mode fails", func(t *testing.T) {
		cfg := base
		cfg.Slack.ChannelAllowlist = []string{"C1"}
		require.Error(t, cfg.Validate(), "a half-edited config must not silently ignore the list")
	})

	t.Run("no served surface fails", func(t *testing.T) {
		cfg := base
		cfg.Slack.DMMode = "ignore"
		cfg.Slack.ChannelMode = "none"
		require.Error(t, cfg.Validate())
		cfg.Slack.DMMode = "redirect"
		require.Error(t, cfg.Validate(), "redirecting DMs to unserved channels is inert")
		cfg.Slack.DMMode = "serve"
		require.NoError(t, cfg.Validate(), "DM-only deployments stay valid")
	})

	t.Run("modes not validated when slack is disabled", func(t *testing.T) {
		cfg := base
		cfg.Slack.Enabled = false
		cfg.Slack.DMMode = "sometimes"
		require.NoError(t, cfg.Validate())
	})
}

func TestLoad_SlackSurfaceEnv(t *testing.T) {
	t.Setenv("KLAUS_GATEWAY_SLACK_DM_MODE", "redirect")
	t.Setenv("KLAUS_GATEWAY_SLACK_CHANNEL_MODE", "allowlist")
	t.Setenv("KLAUS_GATEWAY_SLACK_CHANNEL_ALLOWLIST", "C1, C2,,C3 ")

	cfg, err := config.Load(nil)
	require.NoError(t, err)
	require.Equal(t, config.DMModeRedirect, cfg.Slack.DMMode)
	require.Equal(t, config.ChannelModeAllowlist, cfg.Slack.ChannelMode)
	require.Equal(t, []string{"C1", "C2", "C3"}, cfg.Slack.ChannelAllowlist)
}

func TestValidate_OBO(t *testing.T) {
	base := config.Defaults()
	base.Slack.Enabled = true // OBO links Slack identities, so Slack must be on
	base.OBO = config.OBOConfig{
		Enabled:         true,
		MusterURL:       "https://muster.example.com",
		CallbackBaseURL: "https://gateway.example.com",
		StateKeyFile:    "/etc/obo/state.key",
	}

	t.Run("enabled with required fields is valid", func(t *testing.T) {
		require.NoError(t, base.Validate())
	})

	t.Run("obo without slack fails", func(t *testing.T) {
		cfg := base
		cfg.Slack.Enabled = false
		require.Error(t, cfg.Validate(), "OBO requires the Slack adapter for the email anti-spoof check")
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

func TestA2AConfig_ResolvedRESTURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.A2AConfig
		want string
	}{
		{
			name: "explicit RESTURL wins",
			cfg:  config.A2AConfig{RESTURL: "http://rest.example", URL: "http://a2a.example/api/a2a/kagent"},
			want: "http://rest.example",
		},
		{
			name: "derive through agentgateway keeps the path prefix",
			cfg:  config.A2AConfig{URL: "http://agentgateway.agentic-platform.svc.cluster.local:8080/kagent/api/a2a/kagent"},
			want: "http://agentgateway.agentic-platform.svc.cluster.local:8080/kagent",
		},
		{
			name: "derive direct to controller",
			cfg:  config.A2AConfig{URL: "http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a/kagent"},
			want: "http://kagent-controller.kagent.svc.cluster.local:8083",
		},
		{
			name: "derive from a namespace-less base",
			cfg:  config.A2AConfig{URL: "http://agentgateway.agentic-platform.svc.cluster.local:8080/kagent/api/a2a"},
			want: "http://agentgateway.agentic-platform.svc.cluster.local:8080/kagent",
		},
		{
			name: "derive from a namespace-less base with trailing slash",
			cfg:  config.A2AConfig{URL: "http://agentgateway:8080/kagent/api/a2a/"},
			want: "http://agentgateway:8080/kagent",
		},
		{
			name: "no /api/a2a in URL is not derivable",
			cfg:  config.A2AConfig{URL: "http://agentgateway:8080/kagent"},
			want: "",
		},
		{
			name: "anchored split does not match a stray /api/a2axyz path",
			cfg:  config.A2AConfig{URL: "http://agentgateway:8080/api/a2axyz"},
			want: "",
		},
		{
			name: "both empty",
			cfg:  config.A2AConfig{},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.cfg.ResolvedRESTURL())
		})
	}
}
