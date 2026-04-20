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
			Enabled:     false,
			Mode:        "events",
			SecretsFile: os.ExpandEnv("$HOME/.config/klausctl/gateway/slack-secrets.yaml"),
		},
		CLI: CLIConfig{
			Enabled: false,
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
	fs.BoolVar(&cfg.CLI.Enabled, "cli-enabled", cfg.CLI.Enabled, "Enable the CLI channel adapter at /cli/v1/*.")
	fs.BoolVar(&cfg.Controller, "controller", cfg.Controller, "Enable the embedded ChannelRoute controller (requires --store=crd).")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "klaus-gateway -- channel and routing gateway in front of klaus instances.\n\n")
		fmt.Fprintf(fs.Output(), "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
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
	if v, ok := lookup("CLI_ENABLED"); ok {
		cfg.CLI.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("CONTROLLER"); ok {
		cfg.Controller = strings.EqualFold(v, "true") || v == "1"
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
	if c.Driver == DriverStatic && c.StaticInstances == "" {
		return fmt.Errorf("--static-instances is required with --driver=static")
	}
	return nil
}
