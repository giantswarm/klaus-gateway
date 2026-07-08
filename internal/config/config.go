// Package config resolves runtime configuration from env vars and CLI flags.
//
// Precedence: defaults < KLAUS_GATEWAY_* env < CLI flag. Env vars are read
// first to seed defaults; flags then override any values that were explicitly
// set on the command line. This keeps the binary friendly for both Helm
// (env-driven) and local runs (flag-driven).
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Store names understood by the routing store factory.
const (
	StoreMemory    = "memory"
	StoreBolt      = "bolt"
	StoreConfigMap = "configmap"
	StoreCRD       = "crd"
)

// Driver names understood by the lifecycle manager factory.
const (
	DriverKlausctl = "klausctl"
	DriverOperator = "operator"
	// DriverStatic serves a fixed set of instances declared at startup.
	// Intended for compose / CI smoke harnesses and minimal single-instance
	// deployments where no cluster-side controller is available.
	DriverStatic = "static"
)

// A2AConfig holds runtime configuration for the A2A client surface.
type A2AConfig struct {
	// Enabled gates all A2A behaviour.
	Enabled bool
	// DefaultAgent is the agentRef forwarded to the A2A orchestrator when none
	// is supplied by the inbound channel. Defaults to "klaud-coding".
	DefaultAgent string
	// URL is the base URL of the A2A orchestrator endpoint, without a trailing
	// agent name segment (e.g.
	// http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a/kagent).
	URL string
	// TokenPath is an optional path to a file holding a Bearer token injected
	// as Authorization on every outgoing A2A request. Projected SA tokens
	// refresh automatically; leave empty for unauthenticated in-cluster hops.
	TokenPath string
	// RESTURL is the base URL for the kagent REST API used by the session resume
	// existence-check. Normally the same endpoint as URL (agentgateway fronts
	// both the A2A path and /api/sessions on one host). Empty derives it from URL
	// via ResolvedRESTURL.
	RESTURL string
}

// ResolvedRESTURL returns the kagent REST base URL: RESTURL when set, otherwise
// URL with the /api/a2a/... suffix trimmed so any gateway path prefix is kept
// (e.g. http://agentgateway...:8080/kagent/api/a2a/kagent ->
// http://agentgateway...:8080/kagent). Empty when neither is set or derivable.
func (c A2AConfig) ResolvedRESTURL() string {
	if c.RESTURL != "" {
		return c.RESTURL
	}
	if root, _, ok := strings.Cut(c.URL, "/api/a2a"); ok {
		return root
	}
	return ""
}

// CLIConfig holds runtime configuration for the CLI channel adapter.
type CLIConfig struct {
	// Enabled gates all CLI behaviour; the adapter is skipped when false.
	Enabled bool
}

// SlackConfig holds runtime configuration for the Slack channel adapter.
type SlackConfig struct {
	// Enabled gates all Slack behaviour; the adapter is skipped when false.
	Enabled bool
	// Mode selects the connection method: "events" (Events API webhook,
	// production) or "socketmode" (Socket Mode WebSocket, development).
	Mode string
	// SecretsFile is the path to a YAML file with bot_token, signing_secret,
	// and (for socketmode) app_token. Environment variables (SLACK_BOT_TOKEN
	// etc.) take precedence over file values.
	SecretsFile string
	// DefaultAccessMode controls how new threads start.
	// "locked" = owner-only (default); "open" = anyone; "observe" = locked + forward all.
	// SLACK_DEFAULT_ACCESS_MODE
	DefaultAccessMode string
	// AllowedUsers is a static allow-list of Slack user IDs. When empty and
	// mode is "locked", the first user to message the bot becomes owner.
	// SLACK_ALLOWED_USERS (comma-separated)
	AllowedUsers []string
	// DMOnly restricts the adapter to direct messages: channel messages and
	// @-mentions in channels are ignored. SLACK_DM_ONLY=true. Default false.
	DMOnly bool
	// DropStaleEvents ignores Slack events older than the gateway's start time,
	// so a restart never replays messages queued while it was down.
	// SLACK_DROP_STALE=true. Default false.
	DropStaleEvents bool
	// ProgressMode selects how turn progress is shown: "auto" (default; reactions
	// with a text fallback when reactions:write is unavailable), "reactions", or
	// "text". SLACK_PROGRESS_MODE.
	ProgressMode string
	// WorkingEmoji, DoneEmoji, FailedEmoji override the progress reaction emoji
	// names (no surrounding colons). Empty uses the defaults (eyes /
	// white_check_mark / x). SLACK_WORKING_EMOJI etc.
	WorkingEmoji string
	DoneEmoji    string
	FailedEmoji  string
	// ClearReactionOnDone, when true (the default), removes the working reaction
	// on a successful turn without adding a done reaction, leaving no residual
	// emoji. Set SLACK_CLEAR_REACTION_ON_DONE=false to swap in DoneEmoji instead.
	// The failed reaction is unaffected.
	ClearReactionOnDone bool
}

// OBOConfig configures Slack on-behalf-of (OBO) muster account linking. When
// enabled, the gateway mounts the /auth/slack/link and /auth/slack/callback
// routes (see pkg/auth/musterlink) and forwards a fresh human muster token per
// Slack message instead of acting as a pure machine identity. The gateway is a
// muster OAuth client; humans link once via a browser PKCE flow.
type OBOConfig struct {
	// Enabled gates all OBO behaviour and the linking routes.
	Enabled bool
	// MusterURL is the muster authorization-server base URL. RFC 8414 discovery
	// (/.well-known/oauth-authorization-server) is performed against it.
	MusterURL string
	// ClientID / ClientSecret identify the gateway's muster OAuth client.
	// ClientID is optional: when empty it is derived as CallbackBaseURL +
	// /auth/slack/client.json, the CIMD-document URL the gateway self-hosts (the
	// default and recommended mechanism). Set it only to use a pre-registered
	// client. ClientSecret may be empty for a public (PKCE-only) client.
	ClientID     string
	ClientSecret string
	// CallbackBaseURL is the gateway's public, externally reachable base URL
	// (e.g. https://gateway.example.com). The muster redirect URI is this base
	// joined with the callback path.
	CallbackBaseURL string
	// StorePath is the bolt link-store file (AES-256-GCM encrypted at rest).
	// Empty uses an in-memory store that loses links on restart.
	StorePath string
	// StoreKeyFile holds the 32-byte AES-256 key for the link store. Required
	// when StorePath is set.
	StoreKeyFile string
	// StateKeyFile holds the HMAC key used to sign link state (CSRF + binding
	// the link to the requesting Slack user). Required when OBO is enabled.
	StateKeyFile string
}

// Config is the fully resolved runtime configuration.
type Config struct {
	ListenAddress string
	AdminAddress  string
	LogLevel      string

	Store     string
	BoltPath  string
	Namespace string

	Driver           string
	KlausctlBin      string
	OperatorMCPURL   string
	OperatorMCPToken string
	// StaticInstances is a comma-separated list of `name=baseURL` pairs used
	// by the static driver.
	StaticInstances string

	AgentgatewayURL string

	OTLPEndpoint string

	AutoCreate  bool
	DefaultTTL  time.Duration
	ShowVersion bool

	Slack SlackConfig
	CLI   CLIConfig
	A2A   A2AConfig
	OBO   OBOConfig

	// Controller enables the embedded ChannelRoute controller-runtime manager.
	Controller bool
}

// Defaults returns a Config populated with hard-coded defaults.
func Defaults() Config {
	return Config{
		ListenAddress: ":8080",
		AdminAddress:  ":8081",
		LogLevel:      "info",
		Store:         StoreMemory,
		BoltPath:      "/var/lib/klaus-gateway/routes.bolt",
		Namespace:     "default",
		Driver:        DriverKlausctl,
		KlausctlBin:   "klausctl",
		DefaultTTL:    24 * time.Hour,
		Slack: SlackConfig{
			Enabled:             false,
			Mode:                "events",
			SecretsFile:         os.ExpandEnv("$HOME/.config/klausctl/gateway/slack-secrets.yaml"),
			ClearReactionOnDone: true,
		},
		CLI: CLIConfig{
			Enabled: false,
		},
		A2A: A2AConfig{
			DefaultAgent: "klaud-coding",
		},
	}
}

// Load parses env and flags into a Config. args is typically os.Args[1:].
func Load(args []string) (Config, error) {
	cfg := Defaults()
	applyEnv(&cfg)

	fs := flag.NewFlagSet("klaus-gateway", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddress, "listen-address", cfg.ListenAddress, "Address the public HTTP server binds to.")
	fs.StringVar(&cfg.AdminAddress, "admin-address", cfg.AdminAddress, "Address for /healthz, /readyz, /metrics.")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level: debug, info, warn, error.")
	fs.StringVar(&cfg.Store, "store", cfg.Store, "Routing store: memory, bolt, configmap.")
	fs.StringVar(&cfg.BoltPath, "bolt-path", cfg.BoltPath, "Path to the bolt database (bolt store only).")
	fs.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "Namespace for the configmap store.")
	fs.StringVar(&cfg.Driver, "driver", cfg.Driver, "Lifecycle driver: klausctl, operator, static.")
	fs.StringVar(&cfg.KlausctlBin, "klausctl-bin", cfg.KlausctlBin, "Path to the klausctl binary (klausctl driver only).")
	fs.StringVar(&cfg.OperatorMCPURL, "operator-mcp-url", cfg.OperatorMCPURL, "klaus-operator MCP endpoint (operator driver only).")
	fs.StringVar(&cfg.OperatorMCPToken, "operator-mcp-token", cfg.OperatorMCPToken, "Bearer token for the operator MCP endpoint.")
	fs.StringVar(&cfg.StaticInstances, "static-instances", cfg.StaticInstances, "Static driver instances: name=baseURL[,name=baseURL ...].")
	fs.StringVar(&cfg.AgentgatewayURL, "agentgateway-url", cfg.AgentgatewayURL, "Upstream agentgateway base URL. Empty means direct-to-instance bypass mode.")
	fs.StringVar(&cfg.OTLPEndpoint, "otel-otlp-endpoint", cfg.OTLPEndpoint, "OTLP gRPC endpoint for traces. Empty disables OTel.")
	fs.BoolVar(&cfg.AutoCreate, "auto-create", cfg.AutoCreate, "Create instances on route miss.")
	fs.DurationVar(&cfg.DefaultTTL, "default-ttl", cfg.DefaultTTL, "Default TTL for route entries.")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "Print version information and exit.")
	fs.BoolVar(&cfg.Slack.Enabled, "slack-enabled", cfg.Slack.Enabled, "Enable the Slack channel adapter.")
	fs.StringVar(&cfg.Slack.Mode, "slack-mode", cfg.Slack.Mode, "Slack connection mode: events or socketmode.")
	fs.StringVar(&cfg.Slack.SecretsFile, "slack-secrets-file", cfg.Slack.SecretsFile, "Path to Slack secrets YAML file.")
	fs.StringVar(&cfg.Slack.ProgressMode, "slack-progress-mode", cfg.Slack.ProgressMode, "Slack turn-progress mode: auto (default), reactions, or text.")
	fs.StringVar(&cfg.Slack.WorkingEmoji, "slack-working-emoji", cfg.Slack.WorkingEmoji, "Slack reaction emoji name for a turn in progress (no colons). Empty uses the default.")
	fs.StringVar(&cfg.Slack.DoneEmoji, "slack-done-emoji", cfg.Slack.DoneEmoji, "Slack reaction emoji name for a completed turn (no colons). Empty uses the default.")
	fs.StringVar(&cfg.Slack.FailedEmoji, "slack-failed-emoji", cfg.Slack.FailedEmoji, "Slack reaction emoji name for a failed turn (no colons). Empty uses the default.")
	fs.BoolVar(&cfg.Slack.ClearReactionOnDone, "slack-clear-reaction-on-done", cfg.Slack.ClearReactionOnDone, "On a successful turn, remove the working reaction without adding a done reaction (default true). Set false to swap in the done emoji.")
	fs.BoolVar(&cfg.CLI.Enabled, "cli-enabled", cfg.CLI.Enabled, "Enable the CLI channel adapter at /cli/v1/*.")
	fs.BoolVar(&cfg.Controller, "controller", cfg.Controller, "Enable the embedded ChannelRoute controller (requires --store=crd).")
	fs.BoolVar(&cfg.A2A.Enabled, "a2a-enabled", cfg.A2A.Enabled, "Enable the A2A client surface.")
	fs.StringVar(&cfg.A2A.DefaultAgent, "a2a-default-agent", cfg.A2A.DefaultAgent, "agentRef forwarded to the A2A orchestrator when the channel does not supply one.")
	fs.StringVar(&cfg.A2A.URL, "a2a-url", cfg.A2A.URL, "Base URL of the A2A orchestrator endpoint, without trailing agent name.")
	fs.StringVar(&cfg.A2A.TokenPath, "a2a-token-path", cfg.A2A.TokenPath, "Path to a file holding a Bearer token sent as Authorization on every A2A request (e.g. a projected SA token). Empty disables auth.")
	fs.StringVar(&cfg.A2A.RESTURL, "a2a-rest-url", cfg.A2A.RESTURL, "Base URL for the kagent REST API (session resume check); normally the same agentgateway endpoint as --a2a-url. Empty derives it from --a2a-url.")
	fs.BoolVar(&cfg.OBO.Enabled, "obo-enabled", cfg.OBO.Enabled, "Enable Slack on-behalf-of muster account linking and the /auth/slack/* routes.")
	fs.StringVar(&cfg.OBO.MusterURL, "obo-muster-url", cfg.OBO.MusterURL, "muster authorization-server base URL (RFC 8414 discovery).")
	fs.StringVar(&cfg.OBO.ClientID, "obo-client-id", cfg.OBO.ClientID, "Gateway's muster OAuth client ID. Optional: defaults to the self-hosted CIMD document URL (callback base URL + /auth/slack/client.json).")
	fs.StringVar(&cfg.OBO.ClientSecret, "obo-client-secret", cfg.OBO.ClientSecret, "Gateway's muster OAuth client secret. Empty for a public PKCE client.")
	fs.StringVar(&cfg.OBO.CallbackBaseURL, "obo-callback-base-url", cfg.OBO.CallbackBaseURL, "Gateway's public base URL; the muster redirect URI is this joined with /auth/slack/callback.")
	fs.StringVar(&cfg.OBO.StorePath, "obo-store-path", cfg.OBO.StorePath, "Path to the encrypted bolt link store. Empty uses an in-memory store.")
	fs.StringVar(&cfg.OBO.StoreKeyFile, "obo-store-key-file", cfg.OBO.StoreKeyFile, "Path to the 32-byte AES-256 key file for the link store (required with --obo-store-path).")
	fs.StringVar(&cfg.OBO.StateKeyFile, "obo-state-key-file", cfg.OBO.StateKeyFile, "Path to the HMAC key file used to sign link state (required with --obo-enabled).")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "klaus-gateway -- channel and routing gateway in front of klaus instances.\n\n")
		_, _ = fmt.Fprintf(fs.Output(), "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v, ok := lookup("LISTEN_ADDRESS"); ok {
		cfg.ListenAddress = v
	}
	if v, ok := lookup("ADMIN_ADDRESS"); ok {
		cfg.AdminAddress = v
	}
	if v, ok := lookup("LOG_LEVEL"); ok {
		cfg.LogLevel = v
	}
	if v, ok := lookup("STORE"); ok {
		cfg.Store = v
	}
	if v, ok := lookup("BOLT_PATH"); ok {
		cfg.BoltPath = v
	}
	if v, ok := lookup("NAMESPACE"); ok {
		cfg.Namespace = v
	}
	if v, ok := lookup("DRIVER"); ok {
		cfg.Driver = v
	}
	if v, ok := lookup("KLAUSCTL_BIN"); ok {
		cfg.KlausctlBin = v
	}
	if v, ok := lookup("OPERATOR_MCP_URL"); ok {
		cfg.OperatorMCPURL = v
	}
	if v, ok := lookup("OPERATOR_MCP_TOKEN"); ok {
		cfg.OperatorMCPToken = v
	}
	if v, ok := lookup("STATIC_INSTANCES"); ok {
		cfg.StaticInstances = v
	}
	if v, ok := lookup("AGENTGATEWAY_URL"); ok {
		cfg.AgentgatewayURL = v
	}
	if v, ok := os.LookupEnv("OTEL_EXPORTER_OTLP_ENDPOINT"); ok {
		cfg.OTLPEndpoint = v
	}
	if v, ok := lookup("AUTO_CREATE"); ok {
		cfg.AutoCreate = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("DEFAULT_TTL"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.DefaultTTL = d
		}
	}
	if v, ok := lookup("SLACK_ENABLED"); ok {
		cfg.Slack.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("SLACK_MODE"); ok {
		cfg.Slack.Mode = v
	}
	if v, ok := lookup("SLACK_SECRETS_FILE"); ok {
		cfg.Slack.SecretsFile = v
	}
	if v, ok := lookup("SLACK_DEFAULT_ACCESS_MODE"); ok {
		cfg.Slack.DefaultAccessMode = v
	}
	if v, ok := lookup("SLACK_ALLOWED_USERS"); ok && v != "" {
		for u := range strings.SplitSeq(v, ",") {
			if u = strings.TrimSpace(u); u != "" {
				cfg.Slack.AllowedUsers = append(cfg.Slack.AllowedUsers, u)
			}
		}
	}
	if v, ok := lookup("SLACK_DM_ONLY"); ok {
		cfg.Slack.DMOnly = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("SLACK_DROP_STALE"); ok {
		cfg.Slack.DropStaleEvents = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("SLACK_PROGRESS_MODE"); ok {
		cfg.Slack.ProgressMode = v
	}
	if v, ok := lookup("SLACK_WORKING_EMOJI"); ok {
		cfg.Slack.WorkingEmoji = v
	}
	if v, ok := lookup("SLACK_DONE_EMOJI"); ok {
		cfg.Slack.DoneEmoji = v
	}
	if v, ok := lookup("SLACK_FAILED_EMOJI"); ok {
		cfg.Slack.FailedEmoji = v
	}
	if v, ok := lookup("SLACK_CLEAR_REACTION_ON_DONE"); ok {
		cfg.Slack.ClearReactionOnDone = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("CLI_ENABLED"); ok {
		cfg.CLI.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("CONTROLLER"); ok {
		cfg.Controller = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("A2A_ENABLED"); ok {
		cfg.A2A.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("A2A_DEFAULT_AGENT"); ok {
		cfg.A2A.DefaultAgent = v
	}
	if v, ok := lookup("A2A_URL"); ok {
		cfg.A2A.URL = v
	}
	if v, ok := lookup("A2A_TOKEN_PATH"); ok {
		cfg.A2A.TokenPath = v
	}
	if v, ok := lookup("A2A_REST_URL"); ok {
		cfg.A2A.RESTURL = v
	}
	if v, ok := lookup("OBO_ENABLED"); ok {
		cfg.OBO.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("OBO_MUSTER_URL"); ok {
		cfg.OBO.MusterURL = v
	}
	if v, ok := lookup("OBO_CLIENT_ID"); ok {
		cfg.OBO.ClientID = v
	}
	if v, ok := lookup("OBO_CLIENT_SECRET"); ok {
		cfg.OBO.ClientSecret = v
	}
	if v, ok := lookup("OBO_CALLBACK_BASE_URL"); ok {
		cfg.OBO.CallbackBaseURL = v
	}
	if v, ok := lookup("OBO_STORE_PATH"); ok {
		cfg.OBO.StorePath = v
	}
	if v, ok := lookup("OBO_STORE_KEY_FILE"); ok {
		cfg.OBO.StoreKeyFile = v
	}
	if v, ok := lookup("OBO_STATE_KEY_FILE"); ok {
		cfg.OBO.StateKeyFile = v
	}
}

func lookup(key string) (string, bool) {
	return os.LookupEnv("KLAUS_GATEWAY_" + key)
}

// Validate checks that the config is internally consistent.
func (c Config) Validate() error {
	switch c.Store {
	case StoreMemory, StoreBolt, StoreConfigMap, StoreCRD:
	default:
		return fmt.Errorf("invalid --store %q: must be one of memory, bolt, configmap, crd", c.Store)
	}
	if c.Controller && c.Store != StoreCRD {
		return fmt.Errorf("--controller=true requires --store=crd")
	}
	switch c.Driver {
	case DriverKlausctl, DriverOperator, DriverStatic:
	default:
		return fmt.Errorf("invalid --driver %q: must be one of klausctl, operator, static", c.Driver)
	}
	if c.Store == StoreBolt && c.BoltPath == "" {
		return fmt.Errorf("--bolt-path is required with --store=bolt")
	}
	if c.Driver == DriverOperator && c.OperatorMCPURL == "" {
		return fmt.Errorf("--operator-mcp-url is required with --driver=operator")
	}
	if c.A2A.Enabled && c.A2A.URL == "" {
		return fmt.Errorf("--a2a-url is required with --a2a-enabled")
	}
	if c.OBO.Enabled {
		if !c.Slack.Enabled {
			return fmt.Errorf("--slack-enabled is required with --obo-enabled (OBO links Slack identities and enforces the Slack/muster email match)")
		}
		if c.OBO.MusterURL == "" {
			return fmt.Errorf("--obo-muster-url is required with --obo-enabled")
		}
		if c.OBO.CallbackBaseURL == "" {
			return fmt.Errorf("--obo-callback-base-url is required with --obo-enabled")
		}
		if c.OBO.StateKeyFile == "" {
			return fmt.Errorf("--obo-state-key-file is required with --obo-enabled")
		}
		if (c.OBO.StorePath == "") != (c.OBO.StoreKeyFile == "") {
			return fmt.Errorf("--obo-store-path and --obo-store-key-file must be set together")
		}
	}
	return nil
}
