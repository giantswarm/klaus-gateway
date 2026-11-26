// Package config resolves runtime configuration from env vars and CLI flags.
//
// Precedence: defaults < KLAUS_GATEWAY_* env < CLI flag. Env vars are read
// first to seed defaults; flags then override any values that were explicitly
// set on the command line. This keeps the binary friendly for both Helm
// (env-driven) and local runs (flag-driven).
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
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
	// DefaultAgent is the Agent a channel turn runs on when the channel
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
	// Namespace is the namespace whose Agents are served. Defaults to
	// "kagent".
	Namespace string
	// FallbackIconURLTemplate is used when neither the Agent nor its AgentTemplate
	// carries an icon-URL annotation. "{agent}" is replaced with the agent's
	// technical name. Empty disables the fallback.
	FallbackIconURLTemplate string
	// AgentManagerToolsPrefix is what muster puts before agent-manager's tool
	// names (x_agent-manager_ for start_session as
	// x_agent-manager_start_session). Every Session a thread runs on is started
	// through that tool, as the person behind the turn, so it needs OBO.
	AgentManagerToolsPrefix string
}

// A2AServerConfig holds the A2A server (pkg/a2aserver).
type A2AServerConfig struct {
	// Enabled serves the channel at /a2a and its Agent Card.
	Enabled bool
	// PublicURL is the gateway's public base URL the Agent Card names.
	PublicURL string
	// IssuersFile names the YAML list of trusted issuers Load reads into
	// Issuers.
	IssuersFile string
	// Issuers are the identity providers whose tokens are accepted. It is
	// exclusive with the single-issuer fields Issuer to ExchangeConnectorID.
	Issuers []TrustedIssuer
	// Issuer is the identity provider an A2A client's token is issued by.
	Issuer string
	// JWKSURL is where the issuer's keys are read; empty means Issuer + /keys.
	JWKSURL string
	// Audience is the client an A2A client's token must be issued for.
	Audience string
	// AllowedClients are the clients whose tokens are accepted, read from a
	// token's azp. Empty means any client except ExchangeClientID.
	AllowedClients []string
	// AllowPrivateIPJWKSHosts lists the hostnames whose key set URL may
	// resolve to a private address (TrustedIssuer.AllowPrivateIPJWKSHosts).
	AllowPrivateIPJWKSHosts []string
	// TokenURL is the issuer's token endpoint; empty means Issuer + /token.
	// Without ExchangeTokenURL it is also the exchange's endpoint.
	TokenURL string
	// ExchangeConnectorID names the identity provider connector that
	// verifies an A2A client's token in the exchange.
	ExchangeConnectorID string
	// ExchangeTokenURL is the token endpoint of the identity provider the
	// exchange runs at, whichever issuer the A2A client's token is from.
	ExchangeTokenURL string
	// ExchangeClientID and ExchangeClientSecretFile are the gateway's own
	// confidential client the RFC 8693 exchange authenticates as.
	ExchangeClientID         string
	ExchangeClientSecretFile string
	// ExchangeAudiences are the clients the exchanged token is issued for.
	ExchangeAudiences []string
	// CAFile optionally names a PEM bundle trusted next to the system roots
	// for the exchange's identity provider and for the single issuer.
	CAFile string
	// ToolCallMetadataKeys are the metadata keys an A2A client may send
	// values for through the MCP tool-call metadata A2A extension. Empty does not offer
	// the extension.
	ToolCallMetadataKeys []string
	// MaxBodyBytes caps an /a2a request body; a larger one is refused with
	// 413 before it is decoded.
	MaxBodyBytes int64
}

// DefaultA2AServerMaxBodyBytes is the default /a2a request body cap, 2 MiB.
const DefaultA2AServerMaxBodyBytes = 2 << 20

// TrustedIssuer is one identity provider whose tokens the A2A server accepts.
// A token is matched to the entry by its iss and verified with that entry
// alone.
type TrustedIssuer struct {
	// Name names the issuer's security scheme in the Agent Card.
	Name string `yaml:"name"`
	// Issuer is the token's iss, exactly.
	Issuer string `yaml:"issuer"`
	// JWKSURL is the issuer's key set.
	JWKSURL string `yaml:"jwksUrl"`
	// TokenURL is the issuer's token endpoint. Set, the Agent Card declares
	// the issuer as an OAuth 2.0 client credentials scheme at it.
	TokenURL string `yaml:"tokenUrl"`
	// MetadataURL is the issuer's RFC 8414 authorization server metadata
	// the OAuth 2.0 scheme names. It needs TokenURL.
	MetadataURL string `yaml:"metadataUrl"`
	// OpenIDConnectURL is the issuer's OpenID Connect discovery document.
	// Without TokenURL the Agent Card declares the issuer as an OpenID
	// Connect scheme at it; empty means Issuer +
	// /.well-known/openid-configuration. It excludes TokenURL.
	OpenIDConnectURL string `yaml:"openIdConnectUrl"`
	// Audience is the client a token must be issued for.
	Audience string `yaml:"audience"`
	// AllowedClients are the clients whose tokens are accepted, read from a
	// token's azp. Empty means any client except the exchange client.
	AllowedClients []string `yaml:"allowedClients"`
	// ConnectorID names the connector of the exchange's identity provider
	// that verifies this issuer's tokens.
	ConnectorID string `yaml:"connectorId"`
	// SubjectTokenType is the RFC 8693 token type of the tokens the issuer
	// sends, the exchange's subject_token_type: SubjectTokenIDToken or
	// SubjectTokenAccessToken. Empty means SubjectTokenIDToken.
	SubjectTokenType string `yaml:"subjectTokenType"`
	// CAFile optionally names a PEM bundle trusted for the key set next to
	// the system roots.
	CAFile string `yaml:"caFile"`
	// AllowPrivateIPJWKSHosts lists the hostnames whose key set URL may
	// resolve to a private (RFC 1918, RFC 4193) address, such as an
	// in-cluster identity provider. Every other host, and loopback and
	// link-local addresses always, are refused (mcp-oauth's SSRF guard).
	AllowPrivateIPJWKSHosts []string `yaml:"allowPrivateIpJwksHosts"`
}

// Subject token types an issuer entry may name (RFC 8693 section 3).
const (
	SubjectTokenIDToken     = "urn:ietf:params:oauth:token-type:id_token"     //nolint:gosec // G101: an RFC 8693 identifier, not a credential
	SubjectTokenAccessToken = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec // G101: an RFC 8693 identifier, not a credential
)

// ReadTrustedIssuers reads the YAML list of trusted issuers at path. Unknown
// fields are refused.
func ReadTrustedIssuers(path string) ([]TrustedIssuer, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the issuers file is operator configuration (a chart ConfigMap)
	if err != nil {
		return nil, fmt.Errorf("read the trusted issuers file: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var issuers []TrustedIssuer
	if err := decoder.Decode(&issuers); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse the trusted issuers file %s: %w", path, err)
	}
	return issuers, nil
}

// singleIssuer reports whether any single-issuer field is set.
func (c A2AServerConfig) singleIssuer() bool {
	return c.Issuer != "" || c.JWKSURL != "" || c.Audience != "" || len(c.AllowedClients) > 0 || c.TokenURL != "" || c.ExchangeConnectorID != ""
}

// TrustedIssuers returns Issuers or, without them, the one issuer the
// single-issuer fields describe, named oauth2.
func (c A2AServerConfig) TrustedIssuers() []TrustedIssuer {
	if len(c.Issuers) > 0 || !c.singleIssuer() {
		return c.Issuers
	}
	issuer := strings.TrimSuffix(c.Issuer, "/")
	single := TrustedIssuer{
		Name:                    "oauth2",
		Issuer:                  issuer,
		JWKSURL:                 c.JWKSURL,
		TokenURL:                c.TokenURL,
		Audience:                c.Audience,
		AllowedClients:          c.AllowedClients,
		ConnectorID:             c.ExchangeConnectorID,
		CAFile:                  c.CAFile,
		AllowPrivateIPJWKSHosts: c.AllowPrivateIPJWKSHosts,
	}
	if single.JWKSURL == "" {
		single.JWKSURL = issuer + "/keys"
	}
	if single.TokenURL == "" {
		single.TokenURL = issuer + "/token"
	}
	return []TrustedIssuer{single}
}

// ExchangeEndpoint is the token endpoint the exchange runs at:
// ExchangeTokenURL or, without it, the single issuer's token endpoint.
func (c A2AServerConfig) ExchangeEndpoint() string {
	if c.ExchangeTokenURL != "" || len(c.Issuers) > 0 {
		return c.ExchangeTokenURL
	}
	if c.TokenURL != "" {
		return c.TokenURL
	}
	if c.Issuer == "" {
		return ""
	}
	return strings.TrimSuffix(c.Issuer, "/") + "/token"
}

func (c A2AServerConfig) validate() error {
	switch {
	case c.PublicURL == "":
		return fmt.Errorf("--a2a-server-enabled requires --a2a-server-public-url")
	case c.MaxBodyBytes <= 0:
		return fmt.Errorf("--a2a-server-max-body-bytes must be positive")
	case len(c.Issuers) > 0 && c.singleIssuer():
		return fmt.Errorf("--a2a-server-issuers-file is exclusive with --a2a-server-issuer, --a2a-server-jwks-url, --a2a-server-audience, --a2a-server-allowed-clients, --a2a-server-token-url and --a2a-server-exchange-connector-id")
	case len(c.Issuers) == 0 && (c.Issuer == "" || c.Audience == ""):
		return fmt.Errorf("--a2a-server-enabled requires --a2a-server-issuers-file, or --a2a-server-issuer and --a2a-server-audience")
	case c.ExchangeEndpoint() == "":
		return fmt.Errorf("--a2a-server-issuers-file requires --a2a-server-exchange-token-url")
	case c.ExchangeClientID == "", c.ExchangeClientSecretFile == "", len(c.ExchangeAudiences) == 0:
		return fmt.Errorf("--a2a-server-enabled requires --a2a-server-exchange-client-id, --a2a-server-exchange-client-secret-file and --a2a-server-exchange-audiences")
	}
	names := map[string]bool{}
	issuers := map[string]bool{}
	for i, issuer := range c.TrustedIssuers() {
		switch {
		case issuer.Name == "", issuer.Issuer == "", issuer.JWKSURL == "", issuer.Audience == "":
			return fmt.Errorf("issuer %d: name, issuer, jwksUrl and audience are required", i)
		case names[issuer.Name]:
			return fmt.Errorf("issuer %d: name %q is not unique", i, issuer.Name)
		case issuers[issuer.Issuer]:
			return fmt.Errorf("issuer %d: issuer %q is not unique", i, issuer.Issuer)
		case issuer.MetadataURL != "" && issuer.TokenURL == "":
			return fmt.Errorf("issuer %q: metadataUrl needs tokenUrl: the OAuth 2.0 scheme it belongs to is a client credentials flow", issuer.Name)
		case issuer.OpenIDConnectURL != "" && issuer.TokenURL != "":
			return fmt.Errorf("issuer %q: openIdConnectUrl and tokenUrl are exclusive: the card declares the issuer either as OpenID Connect or as OAuth 2.0", issuer.Name)
		case issuer.SubjectTokenType != "" && issuer.SubjectTokenType != SubjectTokenIDToken && issuer.SubjectTokenType != SubjectTokenAccessToken:
			return fmt.Errorf("issuer %q: subjectTokenType must be %s or %s", issuer.Name, SubjectTokenIDToken, SubjectTokenAccessToken)
		case slices.Contains(issuer.AllowedClients, c.ExchangeClientID):
			return fmt.Errorf("issuer %q: allowed clients must not contain the exchange client %q: its tokens are platform tokens", issuer.Name, c.ExchangeClientID)
		}
		names[issuer.Name] = true
		issuers[issuer.Issuer] = true
	}
	return nil
}

// DefaultSlashCommand is the slash command deploy/slack/manifest.yaml
// declares; an app whose command is named differently sets SlashCommand. The
// Slack adapter keeps the same value as its own default for an Adapter built
// without this package.
const DefaultSlashCommand = "/swarmgeist"

// DefaultAgentManagerToolsPrefix is the prefix of the Agent Platform's
// agent-manager behind muster, whose server is named agent-manager.
const DefaultAgentManagerToolsPrefix = "x_agent-manager_"

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
	// ContextBotIDs lists the Slack bot_ids (B…) whose posts a thread read
	// hands to the agent, the alerting integrations conversations are opened
	// under; every other bot's post is left out. SLACK_CONTEXT_BOT_IDS
	// (comma-separated). Empty shares no bot's post.
	ContextBotIDs []string
	// ContextBotNames lists patterns (path.Match syntax, case-insensitive)
	// matched against a bot's profile name — its Slack app's configured bot
	// name, not the name a post sets — whose posts a thread read hands to the
	// agent beside ContextBotIDs': "*-alertmanager" admits every
	// installation's Alertmanager app. SLACK_CONTEXT_BOT_NAMES
	// (comma-separated). Empty matches no bot by name.
	ContextBotNames []string
	// SlashCommand is the app's slash command as deploy/slack/manifest.yaml
	// declares it, the way a conversation starts; the help and the notices
	// name it. Slack sends the name only with an invocation, so it is
	// configured. SLACK_SLASH_COMMAND. Default /swarmgeist.
	SlashCommand string
	// DropStaleEvents ignores Slack events older than the gateway's start time,
	// so a restart never replays messages queued while it was down.
	// SLACK_DROP_STALE=true. Default false.
	DropStaleEvents bool
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
	// MinTokenLifetime is the least remaining lifetime of the human token a
	// turn is dispatched with: a cached token with less left is refreshed
	// first, because the agent keeps it for the whole turn. Zero uses
	// musterlink.DefaultMinTokenLifetime (25m).
	MinTokenLifetime time.Duration
	// ConnectorsEnabled offers muster's grants in the authorize modal (and
	// *Authorize all* on Connect prompts). Requires Enabled.
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

// RefusalsConfig configures the egress refusal notices: the gateway reads
// the Substrate egress's refusal counter, resolves each refusing Session to
// its Agent, and tells the Agent's creator by direct message, one notice per
// agent and destination within Window.
type RefusalsConfig struct {
	// Enabled starts the watcher. Requires Slack.
	Enabled bool
	// EgressService is the egress's Service as <namespace>/<name>; its
	// EndpointSlices name the pods whose stats port is read.
	EgressService string
	// StatsPort and MetricsPath locate each egress pod's metrics.
	StatsPort   int
	MetricsPath string
	// KagentTarget is the kagent controller's gRPC address (host:port,
	// plaintext), called directly with the token at KagentTokenFile: the
	// controller's session reader admits this gateway's ServiceAccount.
	KagentTarget    string
	KagentTokenFile string
	// Window is how long one notice covers its agent and destination;
	// Interval how often the egress is read.
	Window   time.Duration
	Interval time.Duration
	// AgentURLTemplate opens an agent where its egress is edited;
	// "{namespace}" and "{name}" are replaced. Empty offers no link.
	AgentURLTemplate string
}

// DefaultRefusalsConfig is RefusalsConfig as an installation of the agent
// platform runs it, off.
var DefaultRefusalsConfig = RefusalsConfig{
	EgressService:   "ate-system/atenet-egress",
	StatsPort:       15020,
	MetricsPath:     "/metrics",
	KagentTarget:    "kagent-controller.kagent.svc:8083",
	KagentTokenFile: "/var/run/secrets/kagent/token", // #nosec G101 -- a path, not a credential
	Window:          24 * time.Hour,
	Interval:        time.Minute,
}

// WorkspacesConfig locates the workspace-manager's tools behind muster, which
// the Slack adapter calls as the linked person.
type WorkspacesConfig struct {
	// ToolsPrefix is what muster puts before the workspace-manager's tool
	// names (x_workspace-manager_ for list_workspaces as
	// x_workspace-manager_list_workspaces). Empty turns workspaces off: the
	// gateway never asks the workspace-manager.
	ToolsPrefix string
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

	Slack      SlackConfig
	A2A        A2AConfig
	OBO        OBOConfig
	A2AServer  A2AServerConfig
	Reviews    ReviewsConfig
	Refusals   RefusalsConfig
	Workspaces WorkspacesConfig
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
		ThreadTTL:     channels.DefaultThreadTTL,
		A2AServer:     A2AServerConfig{MaxBodyBytes: DefaultA2AServerMaxBodyBytes},
		Slack: SlackConfig{
			Enabled:             false,
			Mode:                "events",
			SecretsFile:         os.ExpandEnv("$HOME/.config/klausctl/gateway/slack-secrets.yaml"),
			DMMode:              DMModeServe,
			ChannelMode:         ChannelModeAll,
			SlashCommand:        DefaultSlashCommand,
			ClearReactionOnDone: true,
		},
		A2A: A2AConfig{
			DefaultAgent:            "sre-agent",
			Namespace:               "kagent",
			AgentManagerToolsPrefix: DefaultAgentManagerToolsPrefix,
		},
		OBO: OBOConfig{
			StoreSecretName: "klaus-gateway-obo-links",
		},
		Refusals: DefaultRefusalsConfig,
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
	fs.DurationVar(&cfg.ThreadTTL, "thread-ttl", cfg.ThreadTTL, "Sliding lifetime of a channel thread's conversation: its agent, initiator, grants and Session binding. Refreshed on every handled message; after it the conversation has ended and the next mention starts the thread over. The row itself is kept for twice as long, so a reply in a thread that ended gets a notice rather than silence, and is then dropped. It is also the lifetime of the Session share a thread's collaborators run their turns through. 0 means never expire, and the share is then minted without an expiry.")
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
	fs.StringVar(&cfg.Slack.SlashCommand, "slack-slash-command", cfg.Slack.SlashCommand, "The Slack app's slash command, as the manifest declares it; the help and the notices name it as the way to start a conversation.")
	fs.Func("slack-context-bot-ids", "Comma-separated Slack bot_ids (B…) whose posts a thread read hands to the agent; every other bot's post is left out.", func(v string) error {
		cfg.Slack.ContextBotIDs = splitCommaList(v)
		return nil
	})
	fs.Func("slack-context-bot-names", "Comma-separated patterns (path.Match syntax, case-insensitive) matched against a bot's Slack profile name whose posts a thread read hands to the agent, e.g. *-alertmanager.", func(v string) error {
		cfg.Slack.ContextBotNames = splitCommaList(v)
		return nil
	})
	fs.StringVar(&cfg.Slack.WorkingEmoji, "slack-working-emoji", cfg.Slack.WorkingEmoji, "Slack reaction emoji name for a turn in progress (no colons). Empty uses the default.")
	fs.StringVar(&cfg.Slack.DoneEmoji, "slack-done-emoji", cfg.Slack.DoneEmoji, "Slack reaction emoji name for a completed turn (no colons). Empty uses the default.")
	fs.StringVar(&cfg.Slack.FailedEmoji, "slack-failed-emoji", cfg.Slack.FailedEmoji, "Slack reaction emoji name for a failed turn (no colons). Empty uses the default.")
	fs.BoolVar(&cfg.Slack.ClearReactionOnDone, "slack-clear-reaction-on-done", cfg.Slack.ClearReactionOnDone, "On a successful turn, remove the working reaction without adding a done reaction (default true). Set false to swap in the done emoji.")
	fs.BoolVar(&cfg.A2A.Enabled, "a2a-enabled", cfg.A2A.Enabled, "Enable the A2A client surface.")
	fs.StringVar(&cfg.A2A.DefaultAgent, "a2a-default-agent", cfg.A2A.DefaultAgent, "Agent a turn runs on when the channel names none: a bare name in --a2a-namespace, or namespace/name.")
	fs.StringVar(&cfg.A2A.URL, "a2a-url", cfg.A2A.URL, "kagent controller gRPC target through agentgateway: grpc://host:port (h2c) or grpcs://host[:port] (TLS, 443 by default).")
	fs.StringVar(&cfg.A2A.CAFile, "a2a-ca-file", cfg.A2A.CAFile, "PEM bundle trusted for a grpcs:// --a2a-url in addition to the system roots. Empty uses the system roots only.")
	fs.StringVar(&cfg.A2A.Namespace, "a2a-namespace", cfg.A2A.Namespace, "Namespace whose Agents are served.")
	fs.StringVar(&cfg.A2A.AgentManagerToolsPrefix, "a2a-agent-manager-tools-prefix", cfg.A2A.AgentManagerToolsPrefix, "Prefix muster puts before agent-manager's tool names; every Session is started through its start_session as the linked person (requires --obo-enabled).")
	fs.StringVar(&cfg.A2A.FallbackIconURLTemplate, "a2a-fallback-icon-url-template", cfg.A2A.FallbackIconURLTemplate, "Fallback agent icon URL used when neither the Agent nor its AgentTemplate has an icon-URL annotation. \"{agent}\" is replaced with the agent's technical name. Empty disables the fallback.")
	fs.BoolVar(&cfg.OBO.Enabled, "obo-enabled", cfg.OBO.Enabled, "Enable Slack on-behalf-of muster account linking and the /auth/slack/* routes.")
	fs.StringVar(&cfg.OBO.MusterURL, "obo-muster-url", cfg.OBO.MusterURL, "muster authorization-server base URL (RFC 8414 discovery).")
	fs.StringVar(&cfg.OBO.ClientID, "obo-client-id", cfg.OBO.ClientID, "Gateway's muster OAuth client ID. Optional: defaults to the self-hosted CIMD document URL (callback base URL + /auth/slack/client.json).")
	fs.StringVar(&cfg.OBO.ClientSecret, "obo-client-secret", cfg.OBO.ClientSecret, "Gateway's muster OAuth client secret. Empty for a public PKCE client.")
	fs.StringVar(&cfg.OBO.CallbackBaseURL, "obo-callback-base-url", cfg.OBO.CallbackBaseURL, "Gateway's public base URL; the muster redirect URI is this joined with /auth/slack/callback.")
	fs.StringVar(&cfg.OBO.Store, "obo-store", cfg.OBO.Store, "Link-store backend: memory, bolt (a file at --obo-store-path) or secret (one Kubernetes Secret, --obo-store-secret). Empty means bolt when --obo-store-path is set, memory otherwise.")
	fs.StringVar(&cfg.OBO.StorePath, "obo-store-path", cfg.OBO.StorePath, "Path to the encrypted bolt link store (bolt backend). With --obo-store=secret: an existing bolt file to import links from on start.")
	fs.StringVar(&cfg.OBO.StoreKeyFile, "obo-store-key-file", cfg.OBO.StoreKeyFile, "Path to the 32-byte AES-256 key file for the link store (required with the bolt and secret backends). The key the thread rows' Session shares are sealed under is derived from it.")
	fs.StringVar(&cfg.OBO.StoreSecretName, "obo-store-secret", cfg.OBO.StoreSecretName, "Name of the Secret holding the links (secret backend).")
	fs.StringVar(&cfg.OBO.StoreSecretNamespace, "obo-store-secret-namespace", cfg.OBO.StoreSecretNamespace, "Namespace of the link Secret (secret backend). Empty means the pod's own namespace.")
	fs.StringVar(&cfg.OBO.StateKeyFile, "obo-state-key-file", cfg.OBO.StateKeyFile, "Path to the HMAC key file used to sign link state (required with --obo-enabled).")
	fs.DurationVar(&cfg.OBO.MinTokenLifetime, "obo-min-token-lifetime", cfg.OBO.MinTokenLifetime, "Least remaining lifetime of the human token a turn is dispatched with; a cached token with less left is refreshed first, since the agent keeps it for the whole turn. Set it above the longest turn. 0 means 25m; values below 1m are raised to 1m.")
	fs.BoolVar(&cfg.OBO.ConnectorsEnabled, "obo-connectors-enabled", cfg.OBO.ConnectorsEnabled, "Offer muster's grants in the Slack authorize modal (and Authorize all on Connect prompts). Requires --obo-enabled.")
	fs.BoolVar(&cfg.A2AServer.Enabled, "a2a-server-enabled", cfg.A2AServer.Enabled, "Serve the A2A server at /a2a and its Agent Card. Requires --a2a-enabled and --obo-muster-url.")
	fs.StringVar(&cfg.A2AServer.PublicURL, "a2a-server-public-url", cfg.A2AServer.PublicURL, "Public base URL of the gateway the Agent Card names.")
	fs.StringVar(&cfg.A2AServer.IssuersFile, "a2a-server-issuers-file", cfg.A2AServer.IssuersFile, "YAML list of the issuers whose tokens are accepted, each with its name, issuer, jwksUrl, audience, allowedClients, connectorId and optional metadataUrl, tokenUrl and caFile. Exclusive with the single-issuer flags --a2a-server-issuer to --a2a-server-exchange-connector-id.")
	fs.StringVar(&cfg.A2AServer.Issuer, "a2a-server-issuer", cfg.A2AServer.Issuer, "Issuer of the A2A clients' tokens.")
	fs.StringVar(&cfg.A2AServer.JWKSURL, "a2a-server-jwks-url", cfg.A2AServer.JWKSURL, "Key set of the issuer; empty means the issuer + /keys.")
	fs.StringVar(&cfg.A2AServer.Audience, "a2a-server-audience", cfg.A2AServer.Audience, "Client an A2A client's token must be issued for.")
	fs.Func("a2a-server-allowed-clients", "Comma-separated clients whose tokens are accepted (the token's azp); empty means any client except --a2a-server-exchange-client-id.", func(v string) error {
		cfg.A2AServer.AllowedClients = splitCommaList(v)
		return nil
	})
	fs.StringVar(&cfg.A2AServer.TokenURL, "a2a-server-token-url", cfg.A2AServer.TokenURL, "Token endpoint of the issuer, named in the Agent Card; empty means the issuer + /token.")
	fs.StringVar(&cfg.A2AServer.ExchangeTokenURL, "a2a-server-exchange-token-url", cfg.A2AServer.ExchangeTokenURL, "Token endpoint the exchange runs at, for every issuer. Required with --a2a-server-issuers-file; empty with --a2a-server-issuer means --a2a-server-token-url.")
	fs.StringVar(&cfg.A2AServer.ExchangeClientID, "a2a-server-exchange-client-id", cfg.A2AServer.ExchangeClientID, "Confidential client the token exchange authenticates as.")
	fs.StringVar(&cfg.A2AServer.ExchangeClientSecretFile, "a2a-server-exchange-client-secret-file", cfg.A2AServer.ExchangeClientSecretFile, "File holding the exchange client's secret.")
	fs.StringVar(&cfg.A2AServer.ExchangeConnectorID, "a2a-server-exchange-connector-id", cfg.A2AServer.ExchangeConnectorID, "Identity provider connector that verifies an A2A client's token in the exchange.")
	fs.Func("a2a-server-exchange-audiences", "Comma-separated clients the exchanged token is issued for: every audience the platform's components check.", func(v string) error {
		cfg.A2AServer.ExchangeAudiences = splitCommaList(v)
		return nil
	})
	fs.Func("a2a-server-tool-call-metadata-keys", "Comma-separated metadata keys an A2A client may send values for through kagent's MCP tool-call metadata A2A extension; empty does not offer the extension.", func(v string) error {
		cfg.A2AServer.ToolCallMetadataKeys = splitCommaList(v)
		return nil
	})
	fs.Int64Var(&cfg.A2AServer.MaxBodyBytes, "a2a-server-max-body-bytes", cfg.A2AServer.MaxBodyBytes, "Largest /a2a request body in bytes; a larger one is refused with 413 before it is decoded.")
	fs.Func("a2a-server-allow-private-ip-jwks-hosts", "Comma-separated hostnames whose key set URL may resolve to a private address (an in-cluster identity provider) for --a2a-server-issuer; every other host is refused a private address.", func(v string) error {
		cfg.A2AServer.AllowPrivateIPJWKSHosts = splitCommaList(v)
		return nil
	})
	fs.StringVar(&cfg.A2AServer.CAFile, "a2a-server-ca-file", cfg.A2AServer.CAFile, "PEM bundle trusted next to the system roots for the exchange's token endpoint and for --a2a-server-issuer's key set.")
	fs.BoolVar(&cfg.Reviews.Enabled, "reviews-enabled", cfg.Reviews.Enabled, "Enable the team-review endpoint (POST /reviews, POST /notices) for managers authenticated as a Kubernetes ServiceAccount. Requires --slack-enabled and --obo-enabled.")
	fs.StringVar(&cfg.Reviews.Audience, "reviews-audience", cfg.Reviews.Audience, "Token audience the API server checks for the team-review endpoint (default klaus-gateway).")
	fs.Func("reviews-allowed-callers", "Comma-separated ServiceAccounts (system:serviceaccount:<namespace>:<name>) that may post team reviews.", func(v string) error {
		cfg.Reviews.AllowedCallers = splitCommaList(v)
		return nil
	})
	fs.BoolVar(&cfg.Refusals.Enabled, "refusals-enabled", cfg.Refusals.Enabled, "Tell an agent's creator by direct message when the Substrate egress refuses a destination the agent's egress does not declare, one notice per agent and destination within --refusals-window. Requires --slack-enabled.")
	fs.StringVar(&cfg.Refusals.EgressService, "refusals-egress-service", cfg.Refusals.EgressService, "The Substrate egress's Service as <namespace>/<name>; the stats port of every pod behind it is read.")
	fs.IntVar(&cfg.Refusals.StatsPort, "refusals-egress-stats-port", cfg.Refusals.StatsPort, "The egress pods' stats port.")
	fs.StringVar(&cfg.Refusals.MetricsPath, "refusals-egress-metrics-path", cfg.Refusals.MetricsPath, "The metrics path on the egress pods' stats port.")
	fs.StringVar(&cfg.Refusals.KagentTarget, "refusals-kagent-target", cfg.Refusals.KagentTarget, "The kagent controller's gRPC address (host:port, plaintext) the gateway asks, as its own ServiceAccount, which Agent a refusing Session runs.")
	fs.StringVar(&cfg.Refusals.KagentTokenFile, "refusals-kagent-token-file", cfg.Refusals.KagentTokenFile, "The projected ServiceAccount token (audience kagent) sent to the kagent controller.")
	fs.DurationVar(&cfg.Refusals.Window, "refusals-window", cfg.Refusals.Window, "How long one refusal notice covers its agent and destination; later refusals inside it are counted into the notice.")
	fs.DurationVar(&cfg.Refusals.Interval, "refusals-interval", cfg.Refusals.Interval, "How often the egress's refusal counter is read.")
	fs.StringVar(&cfg.Refusals.AgentURLTemplate, "refusals-agent-url-template", cfg.Refusals.AgentURLTemplate, "URL of an agent where its egress is edited, with \"{namespace}\" and \"{name}\" replaced; the notice's Open agent button. Empty offers none.")
	fs.StringVar(&cfg.Workspaces.ToolsPrefix, "workspace-tools-prefix", cfg.Workspaces.ToolsPrefix, "Prefix muster puts before the workspace-manager's tool names (e.g. x_workspace-manager_). Empty turns workspaces off. Requires --obo-enabled: the tools run as the linked person.")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "klaus-gateway -- Slack channel gateway for the Agent Platform's kagent agents.\n\n")
		_, _ = fmt.Fprintf(fs.Output(), "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if cfg.A2AServer.IssuersFile != "" {
		issuers, err := ReadTrustedIssuers(cfg.A2AServer.IssuersFile)
		if err != nil {
			return Config{}, err
		}
		cfg.A2AServer.Issuers = issuers
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
	if v, ok := lookup("SLACK_SLASH_COMMAND"); ok {
		cfg.Slack.SlashCommand = v
	}
	if v, ok := lookup("SLACK_CONTEXT_BOT_IDS"); ok {
		cfg.Slack.ContextBotIDs = splitCommaList(v)
	}
	if v, ok := lookup("SLACK_CONTEXT_BOT_NAMES"); ok {
		cfg.Slack.ContextBotNames = splitCommaList(v)
	}
	if v, ok := lookup("SLACK_DROP_STALE"); ok {
		cfg.Slack.DropStaleEvents = strings.EqualFold(v, "true") || v == "1"
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
	if v, ok := lookup("A2A_AGENT_MANAGER_TOOLS_PREFIX"); ok {
		cfg.A2A.AgentManagerToolsPrefix = v
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
	if v, ok := lookup("REFUSALS_ENABLED"); ok {
		cfg.Refusals.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v, ok := lookup("REFUSALS_EGRESS_SERVICE"); ok {
		cfg.Refusals.EgressService = v
	}
	if v, ok := lookup("REFUSALS_EGRESS_STATS_PORT"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Refusals.StatsPort = n
		}
	}
	if v, ok := lookup("REFUSALS_EGRESS_METRICS_PATH"); ok {
		cfg.Refusals.MetricsPath = v
	}
	if v, ok := lookup("REFUSALS_KAGENT_TARGET"); ok {
		cfg.Refusals.KagentTarget = v
	}
	if v, ok := lookup("REFUSALS_KAGENT_TOKEN_FILE"); ok {
		cfg.Refusals.KagentTokenFile = v
	}
	if v, ok := lookup("REFUSALS_WINDOW"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Refusals.Window = d
		}
	}
	if v, ok := lookup("REFUSALS_INTERVAL"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Refusals.Interval = d
		}
	}
	if v, ok := lookup("REFUSALS_AGENT_URL_TEMPLATE"); ok {
		cfg.Refusals.AgentURLTemplate = v
	}
	if v, ok := lookup("WORKSPACE_TOOLS_PREFIX"); ok {
		cfg.Workspaces.ToolsPrefix = v
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
	if v, ok := lookup("OBO_MIN_TOKEN_LIFETIME"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.OBO.MinTokenLifetime = d
		}
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
		if c.A2A.AgentManagerToolsPrefix == "" {
			return fmt.Errorf("--a2a-agent-manager-tools-prefix is required with --a2a-enabled (every Session is started through agent-manager's start_session)")
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
		for _, p := range c.Slack.ContextBotNames {
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("--slack-context-bot-names: invalid pattern %q: %w", p, err)
			}
		}
		// Slack's command names are a slash and one word; the help quotes the
		// value as typed, so a value Slack could not run must not be served.
		if cmd := c.Slack.SlashCommand; !strings.HasPrefix(cmd, "/") || len(cmd) < 2 || strings.ContainsAny(cmd, " \t\n") {
			return fmt.Errorf("--slack-slash-command must be a slash followed by the command's name, like /swarmgeist (got %q)", cmd)
		}
	}
	if c.OBO.Enabled {
		if !c.Slack.Enabled {
			return fmt.Errorf("--slack-enabled is required with --obo-enabled (OBO links Slack identities and enforces the Slack/muster email match)")
		}
		if c.OBO.MinTokenLifetime < 0 {
			return fmt.Errorf("--obo-min-token-lifetime must not be negative (got %s)", c.OBO.MinTokenLifetime)
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
	if c.A2AServer.Enabled {
		switch {
		case !c.A2A.Enabled:
			return fmt.Errorf("--a2a-server-enabled requires --a2a-enabled")
		case c.OBO.MusterURL == "":
			return fmt.Errorf("--a2a-server-enabled requires --obo-muster-url: Sessions are started through agent-manager")
		}
		if err := c.A2AServer.validate(); err != nil {
			return err
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
	if c.Refusals.Enabled {
		if !c.Slack.Enabled {
			return fmt.Errorf("--slack-enabled is required with --refusals-enabled (a refusal notice is a Slack direct message)")
		}
		if ns, name, ok := strings.Cut(c.Refusals.EgressService, "/"); !ok || ns == "" || name == "" {
			return fmt.Errorf("--refusals-egress-service %q must be <namespace>/<name>", c.Refusals.EgressService)
		}
		if c.Refusals.StatsPort <= 0 || c.Refusals.StatsPort > 65535 {
			return fmt.Errorf("--refusals-egress-stats-port %d is not a port", c.Refusals.StatsPort)
		}
		if c.Refusals.KagentTarget == "" || c.Refusals.KagentTokenFile == "" {
			return fmt.Errorf("--refusals-kagent-target and --refusals-kagent-token-file are required with --refusals-enabled (the gateway asks kagent which Agent a refusing Session runs)")
		}
		if c.Refusals.Window <= 0 || c.Refusals.Interval <= 0 {
			return fmt.Errorf("--refusals-window and --refusals-interval must be positive")
		}
	}
	if c.Workspaces.ToolsPrefix != "" && !c.OBO.Enabled {
		return fmt.Errorf("--obo-enabled is required with --workspace-tools-prefix (the workspace-manager's tools run through muster as the linked person)")
	}
	if c.OBO.ConnectorsEnabled && !c.OBO.Enabled {
		return fmt.Errorf("--obo-enabled is required with --obo-connectors-enabled (the authorize modal reads the linked person's grants)")
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
