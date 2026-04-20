// Package server wires the public HTTP mux and the admin HTTP mux and manages
// their lifecycles.
//
// The public mux hosts channel adapters and the `/v1/{instance}/...` front
// door in the follow-up PR. Today it's an empty chi router: requests return
// 404 cleanly, traces are still emitted, and the middleware stack is already
// in place.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/giantswarm/klaus-gateway/pkg/observability"
)

// DefaultShutdownTimeout is the drain window applied on SIGINT/SIGTERM.
const DefaultShutdownTimeout = 15 * time.Second

// ReadinessFunc reports whether the gateway is ready to serve traffic.
type ReadinessFunc func(ctx context.Context) error

// Options configures the public + admin servers.
type Options struct {
	PublicAddress string
	AdminAddress  string
	Logger        *slog.Logger
	Metrics       *observability.Metrics
	Ready         ReadinessFunc
	Public        http.Handler
}

// Server owns two http.Server instances.
type Server struct {
	opts   Options
	public *http.Server
	admin  *http.Server
}

// New builds the Server. The caller is responsible for calling Run.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = observability.NewMetrics()
	}
	if opts.Ready == nil {
		opts.Ready = func(context.Context) error { return nil }
	}

	public := chi.NewRouter()
	public.Use(RequestID)
	public.Use(AccessLog(opts.Logger))
	public.Use(opts.Metrics.Middleware("public"))

	if opts.Public != nil {
		public.Mount("/", opts.Public)
	} else {
		// Catch-all so middleware (request-id, access log, RED metrics)
		// still runs for requests to the mux before any channel adapters
		// have been mounted. Without this chi returns 404 without invoking
		// the middleware chain.
		public.Handle("/*", http.NotFoundHandler())
	}

	publicHandler := otelhttp.NewHandler(public, "klaus-gateway")

	admin := chi.NewRouter()
	admin.Use(RequestID)
	admin.Use(AccessLog(opts.Logger))
	registerAdminRoutes(admin, opts)

	return &Server{
		opts: opts,
		public: &http.Server{
			Addr:              opts.PublicAddress,
			Handler:           publicHandler,
			ReadHeaderTimeout: 10 * time.Second,
		},
		admin: &http.Server{
			Addr:              opts.AdminAddress,
			Handler:           admin,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

// Run starts both servers and blocks until ctx is cancelled or a server
// fails. It drains both servers on shutdown with DefaultShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 2)

	go func() {
		s.opts.Logger.Info("public server listening", "address", s.public.Addr)
		if err := s.public.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		s.opts.Logger.Info("admin server listening", "address", s.admin.Addr)
		if err := s.admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		s.opts.Logger.Error("server failure", "error", err)
		s.shutdown()
		return err
	}

	s.shutdown()
	return nil
}

func (s *Server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultShutdownTimeout)
	defer cancel()
	if err := s.public.Shutdown(ctx); err != nil {
		s.opts.Logger.Warn("public server shutdown", "error", err)
	}
	if err := s.admin.Shutdown(ctx); err != nil {
		s.opts.Logger.Warn("admin server shutdown", "error", err)
	}
}

// PublicHandler exposes the wrapped public handler for tests.
func (s *Server) PublicHandler() http.Handler { return s.public.Handler }

// AdminHandler exposes the admin handler for tests.
func (s *Server) AdminHandler() http.Handler { return s.admin.Handler }
