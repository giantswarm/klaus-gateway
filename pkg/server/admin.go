package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

func registerAdminRoutes(r chi.Router, opts Options) {
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writePlain(w, http.StatusOK, "ok")
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := opts.Ready(ctx); err != nil {
			writePlain(w, http.StatusServiceUnavailable, "not ready: "+err.Error())
			return
		}
		writePlain(w, http.StatusOK, "ready")
	})
	r.Handle("/metrics", opts.Metrics.Handler())
}

func writePlain(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}
