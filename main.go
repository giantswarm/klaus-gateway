// klaus-gateway is the channel and routing gateway in front of klaus instances.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/giantswarm/klaus-gateway/internal/config"
	"github.com/giantswarm/klaus-gateway/internal/controller"
	"github.com/giantswarm/klaus-gateway/internal/version"
	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/api"
	v1alpha1 "github.com/giantswarm/klaus-gateway/pkg/api/v1alpha1"
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
	configmapstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/configmap"
	crdstore "github.com/giantswarm/klaus-gateway/pkg/routing/store/crd"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store/memory"
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

	// Wire controller-runtime logger to slog so all reconciler output goes to
	// the same structured logger.
	ctrllog.SetLogger(ctrlzap.New(ctrlzap.UseDevMode(cfg.LogLevel == "debug")))

	logger.Info("klaus-gateway starting",
		"version", version.Version(),
		"git_sha", version.GitSHA(),
		"listen_address", cfg.ListenAddress,
		"admin_address", cfg.AdminAddress,
		"store", cfg.Store,
		"driver", cfg.Driver,
		"agentgateway_url", cfg.AgentgatewayURL,
		"controller", cfg.Controller,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTraces, err := observability.SetupTracing(ctx, cfg.OTLPEndpoint, version.Version())
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
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

	if cfg.Controller {
		if err := startController(ctx, manager, logger); err != nil {
			return fmt.Errorf("start controller: %w", err)
		}
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
	}

	var webAdapter *web.Adapter
	if cfg.Web.Enabled {
		webAdapter = &web.Adapter{Logger: logger}
		if cfg.A2A.Enabled {
			webAdapter.DefaultAgent = cfg.A2A.DefaultAgent
		}
		if err := webAdapter.Start(ctx, facade); err != nil {
			return fmt.Errorf("start web adapter: %w", err)
		}
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
			DMMode:              slackchannel.DMMode(cfg.Slack.DMMode),
			ChannelMode:         slackchannel.ChannelMode(cfg.Slack.ChannelMode),
			ChannelAllowlist:    cfg.Slack.ChannelAllowlist,
			DropStaleEvents:     cfg.Slack.DropStaleEvents,
			ProgressMode:        cfg.Slack.ProgressMode,
			WorkingEmoji:        cfg.Slack.WorkingEmoji,
			DoneEmoji:           cfg.Slack.DoneEmoji,
			FailedEmoji:         cfg.Slack.FailedEmoji,
			ClearReactionOnDone: cfg.Slack.ClearReactionOnDone,
		}
		if cfg.A2A.Enabled {
			slackAdapter.DefaultAgent = cfg.A2A.DefaultAgent
		}
		if err := slackAdapter.Start(ctx, facade); err != nil {
			return fmt.Errorf("start slack adapter: %w", err)
		}
		slackAdapter.Mount(publicMux)
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), server.DefaultShutdownTimeout)
			defer cancel()
			if err := slackAdapter.Stop(stopCtx); err != nil {
				logger.Warn("slack adapter stop", "error", err)
			}
		}()
		logger.Info("slack adapter started", "mode", cfg.Slack.Mode)
	}

	if cfg.CLI.Enabled {
		cliAdapter := &cliachannel.Adapter{Logger: logger}
		if cfg.A2A.Enabled {
			cliAdapter.DefaultAgent = cfg.A2A.DefaultAgent
		}
		if err := cliAdapter.Start(ctx, facade); err != nil {
			return fmt.Errorf("start cli adapter: %w", err)
		}
		cliAdapter.Mount(publicMux)
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), server.DefaultShutdownTimeout)
			defer cancel()
			if err := cliAdapter.Stop(stopCtx); err != nil {
				logger.Warn("cli adapter stop", "error", err)
			}
		}()
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
		var fallback pkga2a.TokenSource
		if cfg.A2A.TokenPath != "" {
			fallback = pkga2a.FileTokenSource{Path: cfg.A2A.TokenPath}
		}
		tokenSource := pkga2a.ForwardedTokenSource{Fallback: fallback}
		// With OBO linking enabled, a Slack turn must carry the human's token:
		// the ServiceAccount fallback stays available to the web/cli channels
		// (which may serve anonymous local callers) but is disabled for Slack,
		// so a turn that reaches the executor without a human token fails
		// instead of silently running as the machine identity.
		if cfg.OBO.Enabled {
			tokenSource.ForwardedOnlyChannels = []string{slackchannel.ChannelName}
		}
		facade.Executor = &pkga2a.A2AClient{
			TokenSource:  tokenSource,
			BaseURL:      cfg.A2A.URL,
			DefaultAgent: cfg.A2A.DefaultAgent,
		}
		restURL := cfg.A2A.ResolvedRESTURL()
		if restURL != "" {
			kagentClient := &pkga2a.KagentClient{
				BaseURL:     restURL,
				TokenSource: tokenSource,
			}
			facade.Sessions = kagentClient
			if slackAdapter != nil {
				slackAdapter.Models = kagentClient
				slackAdapter.Roster = kagentClient
			}
		}
		// Card-derived agent branding for Slack: the AgentCard supplies the
		// agent's display name and icon. Cards without an iconUrl fall back to
		// the configured template. Same base and token as the executor.
		if slackAdapter != nil {
			slackAdapter.AgentCards = &pkga2a.AgentCardClient{
				BaseURL:                 cfg.A2A.URL,
				TokenSource:             tokenSource,
				FallbackIconURLTemplate: cfg.A2A.FallbackIconURLTemplate,
			}
		}
		logger.Info("a2a adapter enabled",
			"a2a_url", cfg.A2A.URL,
			"rest_url", restURL,
			"default_agent", cfg.A2A.DefaultAgent,
			"token_path", cfg.A2A.TokenPath,
		)
	}

	apiHandler.Mount(publicMux)
	if webAdapter != nil {
		webAdapter.Mount(publicMux)
	}

	srv := server.New(server.Options{
		PublicAddress: cfg.ListenAddress,
		AdminAddress:  cfg.AdminAddress,
		Logger:        logger,
		Metrics:       metrics,
		Ready:         readiness(routeStore, upstreamClient),
		Public:        publicMux,
	})

	defer func() {
		if webAdapter == nil {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), server.DefaultShutdownTimeout)
		defer cancel()
		if err := webAdapter.Stop(stopCtx); err != nil {
			logger.Warn("web adapter stop", "error", err)
		}
	}()

	return srv.Run(ctx)
}

// startController creates and starts the embedded controller-runtime manager in
// a background goroutine. It returns once the manager's cache is synced.
func startController(ctx context.Context, lm lifecycle.Manager, logger *slog.Logger) error {
	restCfg, err := buildKubeConfig()
	if err != nil {
		return fmt.Errorf("kube config: %w", err)
	}

	scheme := buildScheme()
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         false,
		Metrics:                ctrlmetricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return fmt.Errorf("new manager: %w", err)
	}

	if err := (&controller.ChannelRouteReconciler{
		Client:    mgr.GetClient(),
		Lifecycle: lm,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup reconciler: %w", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			logger.Error("controller manager stopped", "error", err)
		}
	}()

	logger.Info("ChannelRoute controller started")
	return nil
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
	case config.StoreConfigMap:
		restCfg, err := buildKubeConfig()
		if err != nil {
			return nil, fmt.Errorf("configmap store: %w", err)
		}
		kclient, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, fmt.Errorf("configmap store: %w", err)
		}
		return configmapstore.New(kclient, configmapstore.Options{Namespace: cfg.Namespace}), nil
	case config.StoreCRD:
		restCfg, err := buildKubeConfig()
		if err != nil {
			return nil, fmt.Errorf("crd store: %w", err)
		}
		c, err := client.New(restCfg, client.Options{Scheme: buildScheme()})
		if err != nil {
			return nil, fmt.Errorf("crd store: %w", err)
		}
		return crdstore.New(c, cfg.Namespace), nil
	default:
		return nil, fmt.Errorf("unknown store %q", cfg.Store)
	}
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

	var store musterlink.Store
	cleanup := func() error { return nil }
	if cfg.StorePath != "" {
		key, err := os.ReadFile(cfg.StoreKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read obo store key: %w", err)
		}
		bs, err := musterlink.OpenBoltStore(cfg.StorePath, key, logger)
		if err != nil {
			return nil, nil, err
		}
		store, cleanup = bs, bs.Close
	} else {
		store = musterlink.NewMemStore()
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
	return linker, cleanup, nil
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

// buildScheme returns a runtime.Scheme with v1alpha1 types registered.
func buildScheme() *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(s))
	return s
}

// readiness returns 200 once the store is responsive. The upstream URL is
// considered reachable if it parses; a real connect probe lands in the
// follow-up PR alongside the channel adapters.
func readiness(s store.Store, up *upstream.Agentgateway) server.ReadinessFunc {
	return func(ctx context.Context) error {
		if _, err := s.List(ctx); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		_ = up
		return nil
	}
}
