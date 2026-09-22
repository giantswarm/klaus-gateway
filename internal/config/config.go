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
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/a2a"
)

// DMMode selects how Slack direct messages are handled. Mirrors the adapter's
// slack.DMMode; kept separate so this package stays free of adapter imports.
type DMMode string

const (
	DMModeServe    DMMode = "serve"
	DMModeRedirect DMMode = "redirect"
	DMModeIgnore   DMMode = "ignore"
)

// ChannelMode selects which Slack channels are served. Mirrors the adapter's
// slack.ChannelMode.
type ChannelMode string

const (
	ChannelModeAll       ChannelMode = "all"
	ChannelModeAllowlist ChannelMode = "allowlist"
	ChannelModeNone      ChannelMode = "none"
)

// Store names understood by the routing store factory.
const (
	StoreMemory = "memory"
	StoreBolt   = "bolt"
	StoreValkey = "valkey"
)

// A2AConfig holds runtime configuration for the kagent (A2A) client surface.
type A2AConfig struct {
	// Enabled gates all A2A behaviour.
	Enabled bool
	// DefaultAgent is the AgentTemplate a channel turn runs on when the channel
	// names none: a bare name in Namespace, or "namespace/name". Defaults to
	// "sre-agent".
	DefaultAgent string
	// URL is the kagent controller's gRPC target, reached through agentgateway:
	// grpc://host:port (plaintext h2c, the in-cluster agentgateway Service) or
	// grpcs://host[:port] (TLS, the public hostname; 443 when no port is named).
	URL string
	// CAFile optionally names a PEM bundle trusted for a grpcs:// URL in
	// addition to the system roots.
	CAFile string
	// Namespace is the namespace whose AgentTemplates are served. Defaults to
	// "kagent".
	Namespace string
	// FallbackIconURLTemplate is used when an AgentTemplate carries no icon-URL
	// annotation. "{agent}" is replaced with the agent's technical name. Empty
	// disables the fallback.
	FallbackIconURLTemplate string
}

// ValidateURL checks the gRPC target shape of URL, the way the client parses
// it: grpc://host:port or grpcs://host[:port], no path.
func (c A2AConfig) ValidateURL() error {
	if _, _, err := a2a.ParseTarget(c.URL); err != nil {
		return fmt.Errorf("--a2a-url: %w", err)
	}
	return nil
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
	// APIBase overrides the Slack Web API base URL (default
	// https://slack.com/api). A development knob: it points the adapter at a
	// fake Slack for headless proofs of the channel. SLACK_API_BASE.
	APIBase string
	// DMMode selects how direct messages are handled: DMModeServe (answer
	// them, the default), DMModeRedirect (point the user to channels), or
	// DMModeIgnore (drop silently). SLACK_DM_MODE.
	DMMode DMMode
	// ChannelMode selects which channels are served: ChannelModeAll (every
	// channel the bot is invited to, the default), ChannelModeAllowlist
	// (only ChannelAllowlist), or ChannelModeNone (DM-only deployments).
	// SLACK_CHANNEL_MODE.
	ChannelMode ChannelMode
	// ChannelAllowlist lists the Slack channel IDs (C…) served when
	// ChannelMode is "allowlist". SLACK_CHANNEL_ALLOWLIST (comma-separated).
	ChannelAllowlist []string
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
	// Store selects the link-store backend: OBOStoreMemory, OBOStoreBolt or
	// OBOStoreSecret. Empty resolves to bolt when StorePath is set and to
	// memory otherwise (Load does that), so older deployments keep their
	// behaviour without naming a backend.
	Store string
	// StorePath is the bolt link-store file (AES-256-GCM encrypted at rest).
	// Required by the bolt backend. With the Secret backend it is optional and
	// names the bolt file to import links from on start (the migration off the
	// volume); a path that does not exist is skipped.
	StorePath string
	// StoreKeyFile holds the 32-byte AES-256 key for the link store. Required
	// by the bolt and Secret backends.
	StoreKeyFile string
	// StoreSecretName and StoreSecretNamespace locate the Secret of the Secret
	// backend. The name defaults to klaus-gateway-obo-links; an empty namespace
	// means the pod's own namespace.
	StoreSecretName      string
	StoreSecretNamespace string
	// StateKeyFile holds the HMAC key used to sign link state (CSRF + binding
	// the link to the requesting Slack user). Required when OBO is enabled.
	StateKeyFile string
	// ConnectorsEnabled turns on the reactive Slack "Connect <backend>" UX: the
	// gateway detects a core_auth_login challenge in the agent's A2A stream and
	// renders a Connect button from the login link the agent relays. The gateway
	// does not call muster for this. Requires Enabled.
	ConnectorsEnabled bool
}

// OBO link-store backends (OBOConfig.Store).
const (
	OBOStoreMemory = "memory"
	OBOStoreBolt   = "bolt"
	OBOStoreSecret = "secret"
)

// ResolvedStore returns the link-store backend to run: Store when it is set,
// otherwise bolt when StorePath is set and memory when it is not.
func (o OBOConfig) ResolvedStore() string {
	if o.Store != "" {
		return o.Store
	}
	if o.StorePath != "" {
		return OBOStoreBolt
	}
	return OBOStoreMemory
}

// ReviewsConfig configures the inbound team-review endpoint (POST /reviews,
// POST /notices): a manager posts an ask or a notice into a team's channel as
// its own Kubernetes ServiceAccount, verified through the TokenReview API.
type ReviewsConfig struct {
	// Enabled mounts the endpoint. Requires Slack and OBO: the ask is a Slack
	// message and the Approve click runs as the linked person.
	Enabled bool
	// Audience is the token audience the API server checks (TokenReview
	// spec.audiences); the caller projects its token for it. Empty resolves to
	// DefaultReviewsAudience.
	Audience string
	// AllowedCallers lists the ServiceAccounts
	// (system:serviceaccount:<namespace>:<name>) that may post. Nobody else may.
	AllowedCallers []string
}

// DefaultReviewsAudience is the token audience of the team-review endpoint
// when none is configured.
const DefaultReviewsAudience = "klaus-gateway"

// ResolvedAudience returns Audience, or DefaultReviewsAudience when unset.
func (r ReviewsConfig) ResolvedAudience() string {
	if r.Audience != "" {
		return r.Audience
	}
	return DefaultReviewsAudience
}

// Config is the fully resolved runtime configuration.
type Config struct {
	ListenAddress string
	AdminAddress  string
	LogLevel      string

	Store    string
	BoltPath string
	Valkey   ValkeyConfig

	// OTLPEndpoint is the OTLP gRPC collector traces are exported to: a URL
	// (its scheme decides TLS) or a bare host:port (plaintext). Empty exports
	// nothing. OTLPHeaders ride on every export (`key=value,key=value`; the
	// tenant header of a multi-tenant gateway).
	OTLPEndpoint string
	OTLPHeaders  string

	ThreadTTL   time.Duration
	ShowVersion bool

	Slack   SlackConfig
	A2A     A2AConfig
	OBO     OBOConfig
	Reviews ReviewsConfig
}

// ValkeyConfig locates the Valkey server of the valkey routing store
// (StoreValkey): one key per routing entry under KeyPrefix, the entry's TTL as
// the key's expiry. The password comes from PasswordFile when set, otherwise
// from Password (KLAUS_GATEWAY_VALKEY_PASSWORD); it has no flag, so it never
// shows in the process arguments.
type ValkeyConfig struct {
	// URL is the server address as host:port.
	URL string
	// Username is the ACL user; empty means the server's default user.
	Username     string
	Password     string
	PasswordFile string
	// DB is the logical database to SELECT.
	DB int
	// TLS enables TLS to the server; TLSServerName overrides the name the
	// certificate is verified against when it differs from URL's host.
	TLS           bool
	TLSServerName string
	// KeyPrefix namespaces the store's keys; empty means the store's default
	// (klaus-gateway:route:).
	KeyPrefix string
	// Timeout bounds the dial and every command, so a Valkey outage fails a
	// turn within seconds instead of hanging the thread.
	Timeout time.Duration
}

// Defaults returns a Config populated with hard-coded defaults.
func Defaults() Config {
	return Config{
		ListenAddress: ":8080",
		AdminAddress:  ":8081",
		LogLevel:      "info",
		Store:         StoreMemory,
		BoltPath:      "/var/lib/klaus-gateway/routes.bolt",
		Valkey:        ValkeyConfig{Timeout: 2 * time.Second},
		ThreadTTL:     90 * 24 * time.Hour,
		Slack: SlackConfig{
			Enabled:             false,
			Mode:                "events",
			SecretsFile:         os.ExpandEnv("$HOME/.config/klausctl/gateway/slack-secrets.yaml"),
			DMMode:              DMModeServe,
			ChannelMode:         ChannelModeAll,
			ClearReactionOnDone: true,
		},
		A2A: A2AConfig{
			DefaultAgent: "sre-agent",
			Namespace:    "kagent",
		},
		OBO: OBOConfig{
			StoreSecretName: "klaus-gateway-obo-links",
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
	fs.StringVar(&cfg.Store, "store", cfg.Store, "Routing store: memory, bolt, valkey.")
	fs.StringVar(&cfg.BoltPath, "bolt-path", cfg.BoltPath, "Path to the bolt database (bolt store only).")
	fs.StringVar(&cfg.Valkey.URL, "valkey-url", cfg.Valkey.URL, "Valkey server as host:port (valkey store only).")
	fs.StringVar(&cfg.Valkey.Username, "valkey-username", cfg.Valkey.Username, "Valkey ACL user; empty means the default user. The password comes from --valkey-password-file or KLAUS_GATEWAY_VALKEY_PASSWORD.")
	fs.StringVar(&cfg.Valkey.PasswordFile, "valkey-password-file", cfg.Valkey.PasswordFile, "File holding the Valkey password; wins over KLAUS_GATEWAY_VALKEY_PASSWORD.")
	fs.IntVar(&cfg.Valkey.DB, "valkey-db", cfg.Valkey.DB, "Valkey logical database.")
	fs.BoolVar(&cfg.Valkey.TLS, "valkey-tls", cfg.Valkey.TLS, "Connect to Valkey over TLS.")
	fs.StringVar(&cfg.Valkey.TLSServerName, "valkey-tls-server-name", cfg.Valkey.TLSServerName, "Server name the Valkey certificate is verified against when it differs from the URL's host.")
	fs.StringVar(&cfg.Valkey.KeyPrefix, "valkey-key-prefix", cfg.Valkey.KeyPrefix, "Prefix of the routing keys in Valkey; empty means klaus-gateway:route:.")
	fs.DurationVar(&cfg.Valkey.Timeout, "valkey-timeout", cfg.Valkey.Timeout, "Bound on the Valkey dial and on every command.")
	fs.StringVar(&cfg.OTLPEndpoint, "otel-otlp-endpoint", cfg.OTLPEndpoint, "OTLP gRPC endpoint for traces: a URL (http:// plaintext, https:// TLS) or host:port (plaintext). Empty exports no traces.")
	fs.StringVar(&cfg.OTLPHeaders, "otel-otlp-headers", cfg.OTLPHeaders, "Headers sent with every trace export, as key=value,key=value (e.g. X-Scope-OrgID=giantswarm).")
	fs.DurationVar(&cfg.ThreadTTL, "thread-ttl", cfg.ThreadTTL, "Sliding lifetime of a channel thread's conversation: its agent, initiator, grants and AgentInstance binding. Refreshed on every handled message; after it the conversation has ended and the next mention starts the thread over. The row itself is kept for twice as long, so a reply in a thread that ended gets a notice rather than silence, and is then dropped. 0 means never expire.")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "Print version information and exit.")
	fs.BoolVar(&cfg.Slack.Enabled, "slack-enabled", cfg.Slack.Enabled, "Enable the Slack channel adapter.")
	fs.StringVar(&cfg.Slack.Mode, "slack-mode", cfg.Slack.Mode, "Slack connection mode: events or socketmode.")
	fs.StringVar(&cfg.Slack.SecretsFile, "slack-secrets-file", cfg.Slack.SecretsFile, "Path to Slack secrets YAML file.")
	fs.StringVar(&cfg.Slack.APIBase, "slack-api-base", cfg.Slack.APIBase, "Slack Web API base URL override (development: a fake Slack for headless proofs).")
	fs.Func("slack-dm-mode", "Slack DM handling: serve (default), redirect, or ignore.", func(v string) error {
		cfg.Slack.DMMode = DMMode(v)
		return nil
	})
	fs.Func("slack-channel-mode", "Slack channel handling: all (default), allowlist, or none.", func(v string) error {
		cfg.Slack.ChannelMode = ChannelMode(v)
		return nil
	})
	fs.Func("slack-channel-allowlist", "Comma-separated Slack channel IDs served when --slack-channel-mode=allowlist.", func(v string) error {
		cfg.Slack.ChannelAllowlist = splitCommaList(v)
		return nil
	})
	fs.StringVar(&cfg.Slack.ProgressMode, "slack-progress-mode", cfg.Slack.ProgressMode, "Slack turn-progress mode: auto (default), reactions, or text.")
	fs.StringVar(&cfg.Slack.WorkingEmoji, "slack-working-emoji", cfg.Slack.WorkingEmoji, "Slack reaction emoji name for a turn in progress (no colons). Empty uses the default.")
	fs.StringVar(&cfg.Slack.DoneEmoji, "slack-done-emoji", cfg.Slack.DoneEmoji, "Slack reaction emoji name for a completed turn (no colons). Empty uses the default.")
	fs.StringVar(&cfg.Slack.FailedEmoji, "slack-failed-emoji", cfg.Slack.FailedEmoji, "Slack reaction emoji name for a failed turn (no colons). Empty uses the default.")
	fs.BoolVar(&cfg.Slack.ClearReactionOnDone, "slack-clear-reaction-on-done", cfg.Slack.ClearReactionOnDone, "On a successful turn, remove the working reaction without adding a done reaction (default true). Set false to swap in the done emoji.")
	fs.BoolVar(&cfg.A2A.Enabled, "a2a-enabled", cfg.A2A.Enabled, "Enable the A2A client surface.")
	fs.StringVar(&cfg.A2A.DefaultAgent, "a2a-default-agent", cfg.A2A.DefaultAgent, "AgentTemplate a turn runs on when the channel names none: a bare name in --a2a-namespace, or namespace/name.")
	fs.StringVar(&cfg.A2A.URL, "a2a-url", cfg.A2A.URL, "kagent controller gRPC target through agentgateway: grpc://host:port (h2c) or grpcs://host[:port] (TLS, 443 by default).")
	fs.StringVar(&cfg.A2A.CAFile, "a2a-ca-file", cfg.A2A.CAFile, "PEM bundle trusted for a grpcs:// --a2a-url in addition to the system roots. Empty uses the system roots only.")
	fs.StringVar(&cfg.A2A.Namespace, "a2a-namespace", cfg.A2A.Namespace, "Namespace whose AgentTemplates are served.")
	fs.StringVar(&cfg.A2A.FallbackIconURLTemplate, "a2a-fallback-icon-url-template", cfg.A2A.FallbackIconURLTemplate, "Fallback agent icon URL used when the AgentTemplate has no icon-URL annotation. \"{agent}\" is replaced with the agent's technical name. Empty disables the fallback.")
	fs.BoolVar(&cfg.OBO.Enabled, "obo-enabled", cfg.OBO.Enabled, "Enable Slack on-behalf-of muster account linking and the /auth/slack/* routes.")
	fs.StringVar(&cfg.OBO.MusterURL, "obo-muster-url", cfg.OBO.MusterURL, "muster authorization-server base URL (RFC 8414 discovery).")
	fs.StringVar(&cfg.OBO.ClientID, "obo-client-id", cfg.OBO.ClientID, "Gateway's muster OAuth client ID. Optional: defaults to the self-hosted CIMD document URL (callback base URL + /auth/slack/client.json).")
	fs.StringVar(&cfg.OBO.ClientSecret, "obo-client-secret", cfg.OBO.ClientSecret, "Gateway's muster OAuth client secret. Empty for a public PKCE client.")
	fs.StringVar(&cfg.OBO.CallbackBaseURL, "obo-callback-base-url", cfg.OBO.CallbackBaseURL, "Gateway's public base URL; the muster redirect URI is this joined with /auth/slack/callback.")
	fs.StringVar(&cfg.OBO.Store, "obo-store", cfg.OBO.Store, "Link-store backend: memory, bolt (a file at --obo-store-path) or secret (one Kubernetes Secret, --obo-store-secret). Empty means bolt when --obo-store-path is set, memory otherwise.")
	fs.StringVar(&cfg.OBO.StorePath, "obo-store-path", cfg.OBO.StorePath, "Path to the encrypted bolt link store (bolt backend). With --obo-store=secret: an existing bolt file to import links from on start.")
	fs.StringVar(&cfg.OBO.StoreKeyFile, "obo-store-key-file", cfg.OBO.StoreKeyFile, "Path to the 32-byte AES-256 key file for the link store (required with the bolt and secret backends).")
	fs.StringVar(&cfg.OBO.StoreSecretName, "obo-store-secret", cfg.OBO.StoreSecretName, "Name of the Secret holding the links (secret backend).")
	fs.StringVar(&cfg.OBO.StoreSecretNamespace, "obo-store-secret-namespace", cfg.OBO.StoreSecretNamespace, "Namespace of the link Secret (secret backend). Empty means the pod's own namespace.")
	fs.StringVar(&cfg.OBO.StateKeyFile, "obo-state-key-file", cfg.OBO.StateKeyFile, "Path to the HMAC key file used to sign link state (required with --obo-enabled).")
	fs.BoolVar(&cfg.OBO.ConnectorsEnabled, "obo-connectors-enabled", cfg.OBO.ConnectorsEnabled, "Enable the reactive Slack connector UX: the gateway detects a core_auth_login challenge in the agent's response stream and renders a Connect button from the login link the agent relays. The gateway does not call muster. Requires --obo-enabled.")
	fs.BoolVar(&cfg.Reviews.Enabled, "reviews-enabled", cfg.Reviews.Enabled, "Enable the team-review endpoint (POST /reviews, POST /notices) for managers authenticated as a Kubernetes ServiceAccount. Requires --slack-enabled and --obo-enabled.")
	fs.StringVar(&cfg.Reviews.Audience, "reviews-audience", cfg.Reviews.Audience, "Token audience the API server checks for the team-review endpoint (default klaus-gateway).")
	fs.Func("reviews-allowed-callers", "Comma-separated ServiceAccounts (system:serviceaccount:<namespace>:<name>) that may post team reviews.", func(v string) error {
		cfg.Reviews.AllowedCallers = splitCommaList(v)
		return nil
	})

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
	if v, ok := lookup("VALKEY_URL"); ok {
		cfg.Valkey.URL = v
	}
	if v, ok := lookup("VALKEY_USERNAME"); ok {
		cfg.Valkey.Username = v
	}
	if v, ok := lookup("VALKEY_PASSWORD"); ok {
		cfg.Valkey.Password = v
	}
	if v, ok := lookup("VALKEY_PASSWORD_FILE"); ok {
		cfg.Valkey.PasswordFile = v
	}
	if v, ok := lookup("VALKEY_DB"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Valkey.DB = n
		}
	}
	if v, ok := lookup("VALKEY_TLS"); ok {
		cfg.Valkey.TLS = v == "true"
	}
	if v, ok := lookup("VALKEY_TLS_SERVER_NAME"); ok {
		cfg.Valkey.TLSServerName = v
	}
	if v, ok := lookup("VALKEY_KEY_PREFIX"); ok {
		cfg.Valkey.KeyPrefix = v
	}
	if v, ok := lookup("VALKEY_TIMEOUT"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Valkey.Timeout = d
		}
	}
	if v, ok := os.LookupEnv("OTEL_EXPORTER_OTLP_ENDPOINT"); ok {
		cfg.OTLPEndpoint = v
	}
	if v, ok := os.LookupEnv("OTEL_EXPORTER_OTLP_HEADERS"); ok {
		cfg.OTLPHeaders = v
	}
	if v, ok := lookup("THREAD_TTL"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.ThreadTTL = d
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
	if v, ok := lookup("SLACK_API_BASE"); ok {
		cfg.Slack.APIBase = v
	}
	if v, ok := lookup("SLACK_DM_MODE"); ok {
		cfg.Slack.DMMode = DMMode(v)
	}
	if v, ok := lookup("SLACK_CHANNEL_MODE"); ok {
		cfg.Slack.ChannelMode = ChannelMode(v)
	}
	if v, ok := lookup("SLACK_CHANNEL_ALLOWLIST"); ok {
		cfg.Slack.ChannelAllowlist = splitCommaList(v)
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
	if v, ok := lookup("A2A_ENABLED"); ok {
		cfg.A2A.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("A2A_DEFAULT_AGENT"); ok {
		cfg.A2A.DefaultAgent = v
	}
	if v, ok := lookup("A2A_URL"); ok {
		cfg.A2A.URL = v
	}
	if v, ok := lookup("A2A_CA_FILE"); ok {
		cfg.A2A.CAFile = v
	}
	if v, ok := lookup("A2A_NAMESPACE"); ok {
		cfg.A2A.Namespace = v
	}
	if v, ok := lookup("A2A_FALLBACK_ICON_URL_TEMPLATE"); ok {
		cfg.A2A.FallbackIconURLTemplate = v
	}
	if v, ok := lookup("OBO_ENABLED"); ok {
		cfg.OBO.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("OBO_MUSTER_URL"); ok {
		cfg.OBO.MusterURL = v
	}
	if v, ok := lookup("REVIEWS_ENABLED"); ok {
		cfg.Reviews.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("REVIEWS_AUDIENCE"); ok {
		cfg.Reviews.Audience = v
	}
	if v, ok := lookup("REVIEWS_ALLOWED_CALLERS"); ok {
		cfg.Reviews.AllowedCallers = splitCommaList(v)
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
	if v, ok := lookup("OBO_STORE"); ok {
		cfg.OBO.Store = v
	}
	if v, ok := lookup("OBO_STORE_PATH"); ok {
		cfg.OBO.StorePath = v
	}
	if v, ok := lookup("OBO_STORE_SECRET"); ok {
		cfg.OBO.StoreSecretName = v
	}
	if v, ok := lookup("OBO_STORE_SECRET_NAMESPACE"); ok {
		cfg.OBO.StoreSecretNamespace = v
	}
	if v, ok := lookup("OBO_STORE_KEY_FILE"); ok {
		cfg.OBO.StoreKeyFile = v
	}
	if v, ok := lookup("OBO_STATE_KEY_FILE"); ok {
		cfg.OBO.StateKeyFile = v
	}
	if v, ok := lookup("OBO_CONNECTORS_ENABLED"); ok {
		cfg.OBO.ConnectorsEnabled = strings.EqualFold(v, "true") || v == "1"
	}
}

func lookup(key string) (string, bool) {
	return os.LookupEnv("KLAUS_GATEWAY_" + key)
}

// Validate checks that the config is internally consistent.
func (c Config) Validate() error {
	switch c.Store {
	case StoreMemory, StoreBolt, StoreValkey:
	default:
		return fmt.Errorf("invalid --store %q: must be one of memory, bolt, valkey", c.Store)
	}
	if c.Store == StoreBolt && c.BoltPath == "" {
		return fmt.Errorf("--bolt-path is required with --store=bolt")
	}
	if c.Store == StoreValkey {
		if c.Valkey.URL == "" {
			return fmt.Errorf("--valkey-url is required with --store=valkey")
		}
		if c.Valkey.Timeout <= 0 {
			return fmt.Errorf("--valkey-timeout must be positive")
		}
	}
	if c.ThreadTTL < 0 {
		return fmt.Errorf("--thread-ttl must be zero or positive")
	}
	if c.A2A.Enabled {
		if c.A2A.URL == "" {
			return fmt.Errorf("--a2a-url is required with --a2a-enabled")
		}
		if err := c.A2A.ValidateURL(); err != nil {
			return err
		}
		if c.A2A.Namespace == "" {
			return fmt.Errorf("--a2a-namespace is required with --a2a-enabled")
		}
	}
	if c.A2A.FallbackIconURLTemplate != "" && !strings.Contains(c.A2A.FallbackIconURLTemplate, "{agent}") {
		return fmt.Errorf("--a2a-fallback-icon-url-template must contain the \"{agent}\" placeholder")
	}
	if c.Slack.Enabled {
		switch c.Slack.DMMode {
		case "", DMModeServe, DMModeRedirect, DMModeIgnore:
		default:
			return fmt.Errorf("--slack-dm-mode must be serve, redirect, or ignore (got %q)", c.Slack.DMMode)
		}
		switch c.Slack.ChannelMode {
		case "", ChannelModeAll, ChannelModeAllowlist, ChannelModeNone:
		default:
			return fmt.Errorf("--slack-channel-mode must be all, allowlist, or none (got %q)", c.Slack.ChannelMode)
		}
		if c.Slack.ChannelMode == ChannelModeAllowlist && len(c.Slack.ChannelAllowlist) == 0 {
			return fmt.Errorf("--slack-channel-allowlist must be non-empty with --slack-channel-mode=allowlist")
		}
		if c.Slack.ChannelMode != ChannelModeAllowlist && len(c.Slack.ChannelAllowlist) > 0 {
			return fmt.Errorf("--slack-channel-allowlist is set but --slack-channel-mode is %q: set it to allowlist or drop the list", c.Slack.ChannelMode)
		}
		if c.Slack.ChannelMode == ChannelModeNone && c.Slack.DMMode != DMModeServe && c.Slack.DMMode != "" {
			return fmt.Errorf("--slack-channel-mode=none requires --slack-dm-mode=serve: with DMs on %q the bot would have no served surface", c.Slack.DMMode)
		}
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
		switch c.OBO.ResolvedStore() {
		case OBOStoreMemory:
		case OBOStoreBolt:
			if c.OBO.StorePath == "" || c.OBO.StoreKeyFile == "" {
				return fmt.Errorf("--obo-store-path and --obo-store-key-file are required with --obo-store=bolt")
			}
		case OBOStoreSecret:
			if c.OBO.StoreKeyFile == "" {
				return fmt.Errorf("--obo-store-key-file is required with --obo-store=secret")
			}
			if c.OBO.StoreSecretName == "" {
				return fmt.Errorf("--obo-store-secret is required with --obo-store=secret")
			}
		default:
			return fmt.Errorf("invalid --obo-store %q: must be one of memory, bolt, secret", c.OBO.Store)
		}
	}
	if c.Reviews.Enabled {
		if !c.Slack.Enabled || !c.OBO.Enabled {
			return fmt.Errorf("--slack-enabled and --obo-enabled are required with --reviews-enabled (a team review is a Slack message whose Approve click runs as the linked person)")
		}
		if len(c.Reviews.AllowedCallers) == 0 {
			return fmt.Errorf("--reviews-allowed-callers is required with --reviews-enabled (the ServiceAccounts that may post, system:serviceaccount:<namespace>:<name>)")
		}
	}
	if c.OBO.ConnectorsEnabled && !c.OBO.Enabled {
		return fmt.Errorf("--obo-enabled is required with --obo-connectors-enabled (the connector UX renders a Connect button for the linked user from the login link the agent relays)")
	}
	return nil
}

// splitCommaList parses a comma-separated value into trimmed, non-empty
// entries. An empty or all-whitespace input yields nil.
func splitCommaList(v string) []string {
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
