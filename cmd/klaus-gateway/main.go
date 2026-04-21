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
	"github.com/giantswarm/klaus-gateway/pkg/api"
	v1alpha1 "github.com/giantswarm/klaus-gateway/pkg/api/v1alpha1"
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
		if err := startController(ctx, cfg, manager, logger); err != nil {
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

	webAdapter := &web.Adapter{Logger: logger}
	if err := webAdapter.Start(ctx, facade); err != nil {
		return fmt.Errorf("start web adapter: %w", err)
	}

	publicMux := chi.NewRouter()

	if cfg.Slack.Enabled {
		secrets, err := slackchannel.LoadSecrets(cfg.Slack.SecretsFile)
		if err != nil {
			return fmt.Errorf("slack secrets: %w", err)
		}
		slackAdapter := &slackchannel.Adapter{
			Logger:  logger,
			Mode:    cfg.Slack.Mode,
			Secrets: secrets,
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

	apiHandler := &api.Handler{
		Manager:  manager,
		Streamer: instanceClient,
		Logger:   logger,
	}

	apiHandler.Mount(publicMux)
	webAdapter.Mount(publicMux)

	srv := server.New(server.Options{
		PublicAddress: cfg.ListenAddress,
		AdminAddress:  cfg.AdminAddress,
		Logger:        logger,
		Metrics:       metrics,
		Ready:         readiness(routeStore, upstreamClient),
		Public:        publicMux,
	})

	defer func() {
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
func startController(ctx context.Context, cfg config.Config, lm lifecycle.Manager, logger *slog.Logger) error {
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
