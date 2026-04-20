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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/klaus-gateway/internal/config"
	"github.com/giantswarm/klaus-gateway/internal/version"
	"github.com/giantswarm/klaus-gateway/pkg/api"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
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
		client, err := buildKubeClient()
		if err != nil {
			return nil, fmt.Errorf("configmap store: %w", err)
		}
		return configmapstore.New(client, configmapstore.Options{Namespace: cfg.Namespace}), nil
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

func buildKubeClient() (kubernetes.Interface, error) {
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
	return kubernetes.NewForConfig(cfg)
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
