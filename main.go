// klaus-gateway is the channel and routing gateway in front of klaus instances.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-chi/chi/v5"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/klaus-gateway/internal/config"
	"github.com/giantswarm/klaus-gateway/internal/version"
	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/api"
	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	cliachannel "github.com/giantswarm/klaus-gateway/pkg/channels/cli"
	slackchannel "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/channels/web"
	"github.com/giantswarm/klaus-gateway/pkg/instance"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/klausctl"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/operator"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/static"
	"github.com/giantswarm/klaus-gateway/pkg/observability"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
	boltstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/bolt"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
	valkeystore "github.com/giantswarm/klaus-gateway/pkg/routing/store/valkey"
	"github.com/giantswarm/klaus-gateway/pkg/server"
	"github.com/giantswarm/klaus-gateway/pkg/upstream"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "klaus-gateway:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if cfg.ShowVersion {
		fmt.Printf("klaus-gateway %s (%s)\n", version.Version(), version.GitSHA())
		return nil
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	logger.Info("klaus-gateway starting",
		"version", version.Version(),
		"git_sha", version.GitSHA(),
		"listen_address", cfg.ListenAddress,
		"admin_address", cfg.AdminAddress,
		"store", cfg.Store,
		"driver", cfg.Driver,
		"agentgateway_url", cfg.AgentgatewayURL,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	otlpHeaders, err := observability.ParseHeaders(cfg.OTLPHeaders)
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	shutdownTraces, err := observability.SetupTracing(ctx, observability.TracingConfig{
		Endpoint:       cfg.OTLPEndpoint,
		Headers:        otlpHeaders,
		ServiceVersion: version.Version(),
	})
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	if cfg.OTLPEndpoint != "" {
		logger.Info("trace export enabled", "otlp_endpoint", cfg.OTLPEndpoint, "otlp_header_count", len(otlpHeaders))
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), server.DefaultShutdownTimeout)
		defer cancel()
		if err := shutdownTraces(shutdownCtx); err != nil {
			logger.Warn("tracer provider shutdown", "error", err)
		}
	}()

	metrics := observability.NewMetrics()

	routeStore, err := buildStore(cfg)
	if err != nil {
		return fmt.Errorf("build store: %w", err)
	}
	defer func() {
		if err := routeStore.Close(); err != nil {
			logger.Warn("route store close", "error", err)
		}
	}()

	manager, err := buildLifecycle(cfg)
	if err != nil {
		return fmt.Errorf("build lifecycle: %w", err)
	}

	upstreamClient, err := upstream.Parse(cfg.AgentgatewayURL)
	if err != nil {
		return fmt.Errorf("parse agentgateway url: %w", err)
	}

	router := routing.New(routeStore, manager, cfg.AutoCreate, cfg.DefaultTTL)

	instanceClient := instance.NewClient()
	if upstreamClient != nil {
		instanceClient.Upstream = upstreamClient
	}

	facade := &channels.Facade{
		Router:    router,
		Client:    instanceClient,
		Lifecycle: manager,
		Routes:    routeStore,
		// A turn a shutdown cuts short is delivered after the restart only when
		// the thread's record of it outlives the process.
		Durable: cfg.Store != config.StoreMemory,
	}

	// Adapters are stopped in reverse start order once the servers have
	// drained, and before the clients they use are closed (see stopAdapters).
	var adapters []channels.ChannelAdapter

	var webAdapter *web.Adapter
	if cfg.Web.Enabled {
		webAdapter = &web.Adapter{Logger: logger, Turns: metrics}
		if cfg.A2A.Enabled {
			webAdapter.DefaultAgent = cfg.A2A.DefaultAgent
		}
		if err := webAdapter.Start(ctx, facade); err != nil {
			return fmt.Errorf("start web adapter: %w", err)
		}
		adapters = append(adapters, webAdapter)
	}

	publicMux := chi.NewRouter()

	var slackAdapter *slackchannel.Adapter
	if cfg.Slack.Enabled {
		secrets, err := slackchannel.LoadSecrets(cfg.Slack.SecretsFile)
		if err != nil {
			return fmt.Errorf("slack secrets: %w", err)
		}
		slackAdapter = &slackchannel.Adapter{
			Logger:              logger,
			Mode:                cfg.Slack.Mode,
			Secrets:             secrets,
			APIBase:             cfg.Slack.APIBase,
			DMMode:              slackchannel.DMMode(cfg.Slack.DMMode),
			ChannelMode:         slackchannel.ChannelMode(cfg.Slack.ChannelMode),
			ChannelAllowlist:    cfg.Slack.ChannelAllowlist,
			DropStaleEvents:     cfg.Slack.DropStaleEvents,
			ProgressMode:        cfg.Slack.ProgressMode,
			WorkingEmoji:        cfg.Slack.WorkingEmoji,
			DoneEmoji:           cfg.Slack.DoneEmoji,
			FailedEmoji:         cfg.Slack.FailedEmoji,
			ClearReactionOnDone: cfg.Slack.ClearReactionOnDone,
			Turns:               metrics,
		}
		if cfg.A2A.Enabled {
			slackAdapter.DefaultAgent = cfg.A2A.DefaultAgent
			slackAdapter.Namespace = cfg.A2A.Namespace
		}
		if cfg.Store == config.StoreMemory {
			logger.Warn("slack: the routing store is memory, so every thread's agent, initiator, grants and instance binding are lost on a restart; installations run routing.store: valkey")
		}
		if err := slackAdapter.Start(ctx, facade); err != nil {
			return fmt.Errorf("start slack adapter: %w", err)
		}
		slackAdapter.Mount(publicMux)
		adapters = append(adapters, slackAdapter)
		logger.Info("slack adapter started", "mode", cfg.Slack.Mode)
	}

	if cfg.CLI.Enabled {
		cliAdapter := &cliachannel.Adapter{Logger: logger, Turns: metrics}
		if cfg.A2A.Enabled {
			cliAdapter.DefaultAgent = cfg.A2A.DefaultAgent
		}
		if err := cliAdapter.Start(ctx, facade); err != nil {
			return fmt.Errorf("start cli adapter: %w", err)
		}
		cliAdapter.Mount(publicMux)
		adapters = append(adapters, cliAdapter)
		logger.Info("cli adapter started")
	}

	if cfg.OBO.Enabled {
		var slackEmail func(context.Context, string) (string, error)
		var onLinked func(context.Context, string, string)
		if slackAdapter != nil {
			slackEmail = slackAdapter.LookupUserEmail
			onLinked = slackAdapter.OnUserLinked
		}
		linker, closeLinker, err := buildOBOLinker(cfg.OBO, logger, slackEmail, onLinked)
		if err != nil {
			return fmt.Errorf("build obo linker: %w", err)
		}
		defer func() {
			if err := closeLinker(); err != nil {
				logger.Warn("obo link store close", "error", err)
			}
		}()
		linker.RegisterRoutes(publicMux)
		// Wire the linker into the Slack adapter so dispatch mints a fresh human
		// muster token per turn for linked users (OBO). An unlinked user's turn
		// is aborted with a sign-in prompt; it never runs as the M2M
		// ServiceAccount identity.
		if slackAdapter != nil {
			slackAdapter.OBO = linker
		}
		logger.Info("obo linking routes mounted",
			"link_path", musterlink.LinkPath,
			"callback_path", musterlink.CallbackPath,
			"muster_url", cfg.OBO.MusterURL,
			"email_match", slackEmail != nil,
		)

		// Connector UX: when the agent reports a backend needs the user to sign
		// in, the adapter renders a Connect button from the login link the agent
		// relays. The gateway does not call muster for this. The public base URL
		// lets the button carry a post-login redirect back to the gateway's
		// connector landing (prompt rewrite plus auto-resume); muster only honors
		// it when the landing URL is on its post-login redirect allowlist.
		if cfg.OBO.ConnectorsEnabled && slackAdapter != nil {
			slackAdapter.ConnectorPrompts = true
			slackAdapter.PublicBaseURL = cfg.OBO.CallbackBaseURL
			logger.Info("slack connector prompts enabled")
		}
	}

	apiHandler := &api.Handler{
		Manager:  manager,
		Streamer: instanceClient,
		Logger:   logger,
	}

	if cfg.A2A.Enabled {
		// The controller is spoken to as the person behind the turn only: the
		// forwarded Dex id_token is the sole credential, on every channel. A turn
		// without one is refused instead of running as the gateway's machine
		// identity (the ServiceAccount token, when mounted, serves the
		// Klaus-instance paths only).
		tokenSource := pkga2a.ForwardedTokenSource{ForwardedOnlyChannels: []string{slackchannel.ChannelName, web.ChannelName, cliachannel.ChannelName}}
		kagentClient, err := pkga2a.Dial(pkga2a.Config{
			Target:                  cfg.A2A.URL,
			CAFile:                  cfg.A2A.CAFile,
			Namespace:               cfg.A2A.Namespace,
			TokenSource:             tokenSource,
			FallbackIconURLTemplate: cfg.A2A.FallbackIconURLTemplate,
			Logger:                  logger,
		})
		if err != nil {
			return fmt.Errorf("kagent client: %w", err)
		}
		defer func() {
			if err := kagentClient.Close(); err != nil {
				logger.Warn("kagent client close", "error", err)
			}
		}()
		facade.Agent = kagentClient
		if slackAdapter != nil {
			slackAdapter.Models = kagentClient
			slackAdapter.Roster = kagentClient
			slackAdapter.AgentCards = kagentClient
		}
		logger.Info("kagent client enabled",
			"a2a_url", cfg.A2A.URL,
			"namespace", cfg.A2A.Namespace,
			"default_agent", cfg.A2A.DefaultAgent,
			"ca_file", cfg.A2A.CAFile,
		)
	}

	apiHandler.Mount(publicMux)
	if webAdapter != nil {
		webAdapter.Mount(publicMux)
	}
	// Everything a resubscription needs is wired now: the turns the previous
	// process left running are picked up from here.
	if slackAdapter != nil {
		slackAdapter.RecoverTurns()
	}

	srv := server.New(server.Options{
		PublicAddress: cfg.ListenAddress,
		AdminAddress:  cfg.AdminAddress,
		Logger:        logger,
		Metrics:       metrics,
		Ready:         readiness(routeStore, upstreamClient),
		Public:        publicMux,
	})

	err = srv.Run(ctx)
	stopAdapters(adapters, logger)
	return err
}

// stopAdapters stops the channel adapters in reverse start order, once the
// servers have drained and before the deferred closes take the kagent client,
// the link store and the routing store away: a Slack turn the shutdown cuts
// short still posts its notice, and a /stop-issued cancel still reaches the
// controller. All adapters share one budget, so the pod's termination grace
// has to cover the server drain plus this stop (both DefaultShutdownTimeout).
func stopAdapters(adapters []channels.ChannelAdapter, logger *slog.Logger) {
	stopCtx, cancel := context.WithTimeout(context.Background(), server.DefaultShutdownTimeout)
	defer cancel()
	for i := len(adapters) - 1; i >= 0; i-- {
		if err := adapters[i].Stop(stopCtx); err != nil {
			logger.Warn("adapter stop", "adapter", adapters[i].Name(), "error", err)
		}
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func buildStore(cfg config.Config) (store.Store, error) {
	switch cfg.Store {
	case config.StoreMemory:
		return memory.New(), nil
	case config.StoreBolt:
		return boltstore.Open(cfg.BoltPath)
	case config.StoreValkey:
		password, err := valkeyPassword(cfg.Valkey)
		if err != nil {
			return nil, fmt.Errorf("valkey store: %w", err)
		}
		var tlsCfg *tls.Config
		if cfg.Valkey.TLS {
			tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.Valkey.TLSServerName}
		}
		return valkeystore.New(valkeystore.Options{
			URL:       cfg.Valkey.URL,
			Username:  cfg.Valkey.Username,
			Password:  password,
			DB:        cfg.Valkey.DB,
			TLS:       tlsCfg,
			KeyPrefix: cfg.Valkey.KeyPrefix,
			Timeout:   cfg.Valkey.Timeout,
		})
	default:
		return nil, fmt.Errorf("unknown store %q", cfg.Store)
	}
}

// valkeyPassword is the store's password: the file when one is named (a Secret
// mount; surrounding whitespace trimmed), otherwise the environment's value.
func valkeyPassword(cfg config.ValkeyConfig) (string, error) {
	if cfg.PasswordFile == "" {
		return cfg.Password, nil
	}
	raw, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// buildOBOLinker constructs the muster account-linking Linker for Slack OBO. It
// returns a cleanup func that closes the link store (a no-op for the in-memory
// store). slackEmail is the anti-spoof email lookup; nil skips the email-match
// check at callback. onLinked is invoked after a successful link (nil to skip);
// the Slack adapter uses it to replace the sign-in prompt. The OAuth client_id
// and redirect URI are derived from CallbackBaseURL by musterlink (CIMD); an
// explicit cfg.ClientID overrides it.
func buildOBOLinker(cfg config.OBOConfig, logger *slog.Logger,
	slackEmail func(context.Context, string) (string, error),
	onLinked func(ctx context.Context, slackUserID, email string),
) (*musterlink.Linker, func() error, error) {
	stateKey, err := os.ReadFile(cfg.StateKeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read obo state key: %w", err)
	}

	store, cleanup, err := buildOBOStore(cfg, logger)
	if err != nil {
		return nil, nil, err
	}

	linker, err := musterlink.New(musterlink.Config{
		BaseURL:       cfg.MusterURL,
		ClientID:      cfg.ClientID,
		ClientSecret:  cfg.ClientSecret,
		PublicBaseURL: cfg.CallbackBaseURL,
		StateKey:      stateKey,
		Store:         store,
		SlackEmail:    slackEmail,
		OnLinked:      onLinked,
		Logger:        logger,
	})
	if err != nil {
		_ = cleanup()
		return nil, nil, err
	}
	closeAll := func() error {
		// Write the links the store has not taken yet before the store closes.
		if err := linker.Close(); err != nil {
			logger.Warn("obo: closing linker", "err", err)
		}
		return cleanup()
	}
	return linker, closeAll, nil
}

// buildOBOStore opens the link store cfg selects (see config.OBOConfig.Store)
// and returns it with its close func. The Secret backend is checked once here
// so a missing Secret or Role fails the start instead of leaving every user
// unlinked; when a bolt file is configured next to it, its links are imported
// first (the file is read, never written) so nobody signs in again after the
// move off the volume.
func buildOBOStore(cfg config.OBOConfig, logger *slog.Logger) (musterlink.Store, func() error, error) {
	noop := func() error { return nil }
	switch backend := cfg.ResolvedStore(); backend {
	case config.OBOStoreMemory:
		return musterlink.NewMemStore(), noop, nil
	case config.OBOStoreBolt:
		key, err := os.ReadFile(cfg.StoreKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read obo store key: %w", err)
		}
		bs, err := musterlink.OpenBoltStore(cfg.StorePath, key, logger)
		if err != nil {
			return nil, nil, err
		}
		return bs, bs.Close, nil
	case config.OBOStoreSecret:
		key, err := os.ReadFile(cfg.StoreKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read obo store key: %w", err)
		}
		restCfg, err := buildKubeConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("obo secret store: %w", err)
		}
		kclient, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("obo secret store: %w", err)
		}
		namespace := cfg.StoreSecretNamespace
		if namespace == "" {
			namespace = podNamespace()
		}
		ss, err := musterlink.NewSecretStore(kclient, key, musterlink.SecretStoreOptions{Namespace: namespace, Name: cfg.StoreSecretName}, logger)
		if err != nil {
			return nil, nil, err
		}
		if _, err := ss.Check(); err != nil {
			return nil, nil, fmt.Errorf("obo secret store: %w (the chart renders the Secret and a Role granting get/update/patch on it)", err)
		}
		importBoltLinks(cfg.StorePath, key, ss, logger)
		links, err := ss.Check()
		if err != nil {
			return nil, nil, fmt.Errorf("obo secret store: %w", err)
		}
		logger.Info("obo link store ready", "backend", backend, "secret", ss.Ref(), "links", links)
		return ss, noop, nil
	default:
		return nil, nil, fmt.Errorf("unknown obo store %q", backend)
	}
}

// importBoltLinks runs the one-time bolt -> Secret import when a bolt file is
// configured and present. A failure is logged, not fatal: the Secret backend
// works without it and the file stays for the next start to try again.
func importBoltLinks(path string, key []byte, dst *musterlink.SecretStore, logger *slog.Logger) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		logger.Info("obo link store: no bolt file to import", "path", path)
		return
	}
	added, total, err := musterlink.ImportBoltFile(path, key, dst, logger)
	if err != nil {
		logger.Error("obo link store: bolt import failed", "path", path, "err", err)
		return
	}
	logger.Info("obo link store: imported links from bolt file", "path", path, "imported", added, "total", total, "secret", dst.Ref())
}

// podNamespace is the namespace this pod runs in per the mounted ServiceAccount
// token, or "default" outside a cluster.
func podNamespace() string {
	if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if v := strings.TrimSpace(string(ns)); v != "" {
			return v
		}
	}
	return "default"
}

func buildLifecycle(cfg config.Config) (lifecycle.Manager, error) {
	switch cfg.Driver {
	case config.DriverKlausctl:
		return klausctl.New(cfg.KlausctlBin)
	case config.DriverOperator:
		return operator.New(cfg.OperatorMCPURL, cfg.OperatorMCPToken)
	case config.DriverStatic:
		return static.New(cfg.StaticInstances)
	default:
		return nil, fmt.Errorf("unknown driver %q", cfg.Driver)
	}
}

// buildKubeConfig returns a *rest.Config using in-cluster config when running
// inside Kubernetes, falling back to the local kubeconfig otherwise.
func buildKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		if !errors.Is(err, rest.ErrNotInCluster) {
			return nil, err
		}
		loader := clientcmd.NewDefaultClientConfigLoadingRules()
		cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{})
		cfg, err = cc.ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// readiness returns 200 once the store is responsive: a store that can ping
// its server (valkey) is pinged, any other is listed. The upstream URL is
// considered reachable if it parses; a real connect probe lands in the
// follow-up PR alongside the channel adapters.
func readiness(s store.Store, up *upstream.Agentgateway) server.ReadinessFunc {
	return func(ctx context.Context) error {
		if p, ok := s.(interface{ Ping(context.Context) error }); ok {
			if err := p.Ping(ctx); err != nil {
				return fmt.Errorf("store: %w", err)
			}
			_ = up
			return nil
		}
		if _, err := s.List(ctx); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		_ = up
		return nil
	}
}
