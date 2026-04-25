// Package cli is the HTTP channel adapter for remote klausctl users.
//
// Surface:
//
//	POST /cli/v1/{instance}/run      -- stream a completion as SSE deltas
//	POST /cli/v1/{instance}/messages -- fetch message history for a session
//	GET  /cli/v1/healthz             -- 200 once Start has run
//
// The {instance} path segment is used as the ChannelID in the routing key.
// SessionID from the request body is the ThreadID. Identity is resolved from
// the optional Authorization Bearer header, with the userId body field as a
// stable fallback.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
)

// ChannelName identifies the CLI adapter in routing keys.
const ChannelName = "cli"

// Adapter implements channels.ChannelAdapter for the CLI channel.
type Adapter struct {
	Logger *slog.Logger

	gw      channels.Gateway
	started atomic.Bool
}

// Name returns the adapter name used in routing keys.
func (a *Adapter) Name() string { return ChannelName }

// Start records the gateway facade and marks the adapter ready.
func (a *Adapter) Start(_ context.Context, gw channels.Gateway) error {
	if gw == nil {
		return errors.New("cli: nil gateway")
	}
	a.gw = gw
	a.started.Store(true)
	return nil
}

// Stop is a no-op; the adapter owns no goroutines.
func (a *Adapter) Stop(context.Context) error {
	a.started.Store(false)
	return nil
}

// Mount attaches /cli/v1/* to r. Must be called after Start.
func (a *Adapter) Mount(r chi.Router) {
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
	r.Route("/cli/v1", func(r chi.Router) {
		r.Get("/healthz", a.healthz)
		r.Post("/{instance}/run", a.postRun)
		r.Post("/{instance}/messages", a.postMessages)
	})
}

const maxInboundBytes = 4 << 20 // 4 MiB

type runRequest struct {
	Text      string `json:"text"`
	SessionID string `json:"sessionId"`
	UserID    string `json:"userId,omitempty"`
}

type messagesRequest struct {
	SessionID string `json:"sessionId"`
	UserID    string `json:"userId,omitempty"`
}

func (a *Adapter) healthz(w http.ResponseWriter, _ *http.Request) {
	if !a.started.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok")
}

func (a *Adapter) postRun(w http.ResponseWriter, r *http.Request) {
	if !a.started.Load() {
		http.Error(w, "cli adapter not started", http.StatusServiceUnavailable)
		return
	}

	instanceName := chi.URLParam(r, "instance")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxInboundBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(body)) > maxInboundBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds limit")
		return
	}

	var in runRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	if in.Text == "" || in.SessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "text and sessionId are required")
		return
	}

	userID, subject := extractIdentity(r, in.UserID)

	msg := channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: instanceName,
		UserID:    userID,
		ThreadID:  in.SessionID,
		Text:      in.Text,
		Subject:   subject,
	}

	ref, err := a.gw.Resolve(r.Context(), msg)
	if err != nil {
		a.resolveError(w, err)
		return
	}

	deltas, err := a.gw.SendCompletion(r.Context(), ref, msg)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "send completion: "+err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "response writer does not support streaming")
		return
	}
	setSSEHeaders(w.Header())
	w.Header().Set("X-Klaus-Instance", ref.Name)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	for d := range deltas {
		if d.Err != nil {
			writeSSEError(w, flusher, d.Err)
			return
		}
		if d.Done {
			_, _ = fmt.Fprintf(w, "event: done\ndata: {}\n\n")
			flusher.Flush()
			continue
		}
		if d.Content == "" {
			continue
		}
		if _, err := io.WriteString(w, "data: "); err != nil {
			return
		}
		if err := enc.Encode(map[string]string{"content": d.Content}); err != nil {
			return
		}
		// enc.Encode writes a trailing newline; SSE needs the blank line after.
		if _, err := io.WriteString(w, "\n"); err != nil {
			return
		}
		flusher.Flush()
	}
}

func (a *Adapter) postMessages(w http.ResponseWriter, r *http.Request) {
	if !a.started.Load() {
		http.Error(w, "cli adapter not started", http.StatusServiceUnavailable)
		return
	}

	instanceName := chi.URLParam(r, "instance")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxInboundBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(body)) > maxInboundBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds limit")
		return
	}

	var in messagesRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	if in.SessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "sessionId is required")
		return
	}

	userID, subject := extractIdentity(r, in.UserID)

	ref, err := a.gw.Resolve(r.Context(), channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: instanceName,
		UserID:    userID,
		ThreadID:  in.SessionID,
		Subject:   subject,
	})
	if err != nil {
		a.resolveError(w, err)
		return
	}

	history, err := a.gw.FetchHistory(r.Context(), ref)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "fetch history: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"messages": history})
}

func (a *Adapter) resolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, routing.ErrRouteNotFound) {
		writeJSONError(w, http.StatusNotFound, "no instance bound to this session and auto-create is disabled")
		return
	}
	a.Logger.Error("cli: resolve failed", "error", err)
	writeJSONError(w, http.StatusBadGateway, "resolve: "+err.Error())
}

// extractIdentity returns the (userID, subject) pair for a request.
// The Authorization Bearer value becomes the subject for downstream auth
// passthrough. The body userId field is the stable routing identity.
func extractIdentity(r *http.Request, bodyUserID string) (string, string) {
	var subject string
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		subject = strings.TrimPrefix(auth, "Bearer ")
	}
	if bodyUserID != "" {
		return bodyUserID, subject
	}
	if subject != "" {
		return subject, subject
	}
	return "anonymous", ""
}

func setSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, err error) {
	_, _ = fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
	flusher.Flush()
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": http.StatusText(code)},
	})
}
