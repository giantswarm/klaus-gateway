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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/klaus-gateway/internal/config"
	"github.com/giantswarm/klaus-gateway/internal/version"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/klausctl"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle/operator"
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
	_ = router // wired to the public mux in the follow-up PR
	_ = upstreamClient

	srv := server.New(server.Options{
		PublicAddress: cfg.ListenAddress,
		AdminAddress:  cfg.AdminAddress,
		Logger:        logger,
		Metrics:       metrics,
		Ready:         readiness(routeStore, upstreamClient),
	})

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
