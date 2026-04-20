package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/giantswarm/klaus-gateway/pkg/project"
)

func main() {
	var (
		showVersion bool
		listenAddr  string
		adminAddr   string
		upstream    string
		logLevel    string
	)

	flag.BoolVar(&showVersion, "version", false, "Print version information and exit.")
	flag.StringVar(&listenAddr, "listen-address", ":8080", "Address the public HTTP server binds to.")
	flag.StringVar(&adminAddr, "admin-address", ":8081", "Address for /healthz, /readyz, /metrics.")
	flag.StringVar(&upstream, "agentgateway-url", "", "Upstream agentgateway base URL. Empty means direct-to-instance bypass mode.")
	flag.StringVar(&logLevel, "log-level", "info", "Log level: debug, info, warn, error.")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "klaus-gateway %s -- channel and routing gateway in front of klaus instances.\n\n", project.Version())
		fmt.Fprintf(os.Stderr, "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if showVersion {
		fmt.Printf("klaus-gateway %s (%s)\n", project.Version(), project.GitSHA())
		return
	}

	level := parseLogLevel(logLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	logger.Info("klaus-gateway starting",
		"version", project.Version(),
		"git_sha", project.GitSHA(),
		"listen_address", listenAddr,
		"admin_address", adminAddr,
		"agentgateway_url", upstream,
	)

	logger.Warn("server implementation not yet wired up; see Phase 1 of the architecture plan")
	os.Exit(0)
}

func parseLogLevel(raw string) slog.Level {
	switch raw {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
