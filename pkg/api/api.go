// Package api serves the OpenAI-compatible front door at
// /v1/{instance}/chat/completions and /v1/{instance}/chat/messages.
//
// The path is scoped by instance name so OpenAI SDKs work by only setting
// `baseURL` -- no custom header or query parameter required. The instance
// name is resolved via the lifecycle manager; the routing table is bypassed
// on this path because clients already know the instance they want.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/giantswarm/klaus-gateway/pkg/instance"
	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
)

// Manager is the slice of lifecycle.Manager the API needs.
type Manager interface {
	Get(ctx context.Context, name string) (lifecycle.InstanceRef, error)
}

// Streamer is the slice of instance.Client the API needs.
type Streamer interface {
	StreamCompletion(ctx context.Context, ref lifecycle.InstanceRef, body []byte) (io.ReadCloser, error)
	Messages(ctx context.Context, ref lifecycle.InstanceRef, threadID string) (instance.MessagesResponse, error)
}

// Handler mounts the OpenAI-compat routes on a chi router.
type Handler struct {
	Manager  Manager
	Streamer Streamer
	Logger   *slog.Logger
	// MaxRequestBytes caps inbound body size. Zero means 1MiB default.
	MaxRequestBytes int64
}

// Mount attaches /v1/{instance}/* to r.
func (h *Handler) Mount(r chi.Router) {
	if h.Logger == nil {
		h.Logger = slog.Default()
	}
	r.Post("/v1/{instance}/chat/completions", h.completions)
	r.Post("/v1/{instance}/chat/messages", h.messages)
}

const defaultMaxRequestBytes = 1 << 20 // 1 MiB

func (h *Handler) completions(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "instance")
	ref, ok := h.resolve(w, r, name)
	if !ok {
		return
	}

	limit := h.MaxRequestBytes
	if limit <= 0 {
		limit = defaultMaxRequestBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds limit")
		return
	}

	src, err := h.Streamer.StreamCompletion(r.Context(), ref, body)
	if err != nil {
		h.upstreamError(w, r, err, "stream completion")
		return
	}
	defer src.Close()

	if err := instance.ProxySSE(r.Context(), w, src); err != nil {
		// Headers are already sent; log and return. Client either
		// disconnected or the upstream closed mid-stream.
		h.Logger.Warn("sse proxy terminated", "error", err, "instance", name)
	}
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "instance")
	ref, ok := h.resolve(w, r, name)
	if !ok {
		return
	}

	threadID := r.URL.Query().Get("thread_id")
	if threadID == "" {
		// Tolerate the OpenAI-style body: {"thread_id": "..."}. Empty body
		// is also fine -- the instance will return the default thread.
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if len(bytes.TrimSpace(body)) > 0 {
			var in struct {
				ThreadID string `json:"thread_id"`
			}
			if err := json.Unmarshal(body, &in); err != nil {
				writeError(w, http.StatusBadRequest, "parse body: "+err.Error())
				return
			}
			threadID = in.ThreadID
		}
	}

	resp, err := h.Streamer.Messages(r.Context(), ref, threadID)
	if err != nil {
		h.upstreamError(w, r, err, "messages")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, name string) (lifecycle.InstanceRef, bool) {
	if name == "" {
		writeError(w, http.StatusBadRequest, "instance name is required")
		return lifecycle.InstanceRef{}, false
	}
	ref, err := h.Manager.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, lifecycle.ErrNotFound) {
			writeError(w, http.StatusNotFound, "instance "+name+" not found")
			return lifecycle.InstanceRef{}, false
		}
		h.Logger.Error("lifecycle get", "error", err, "instance", name)
		writeError(w, http.StatusBadGateway, "lifecycle lookup failed")
		return lifecycle.InstanceRef{}, false
	}
	return ref, true
}

func (h *Handler) upstreamError(w http.ResponseWriter, r *http.Request, err error, op string) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		// Client went away; no point writing a response body.
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, op+": upstream timed out")
		return
	}
	h.Logger.Error("upstream call failed", "op", op, "error", err)
	// Status messages from the instance client embed the HTTP status; pass a
	// 502 with a trimmed snippet to aid debugging.
	msg := err.Error()
	if len(msg) > 512 {
		msg = msg[:512]
	}
	writeError(w, http.StatusBadGateway, op+": "+strings.TrimSpace(msg))
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    http.StatusText(code),
		},
	})
}
