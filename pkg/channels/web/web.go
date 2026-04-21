// Package web is the HTTP channel adapter that the lab webapp (and any
// other bytes-in / SSE-out consumer) calls into.
//
// Surface:
//
//	POST /web/messages     -- send one user message, receive deltas as SSE
//	GET  /web/messages     -- fetch history for (channelId, userId, threadId)
//	GET  /web/healthz      -- 200 once Start has run
//
// The adapter is channel-agnostic on the wire: it normalises requests into
// channels.InboundMessage and hands off to the Gateway facade.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
)

// ChannelName identifies the web adapter in routing keys.
const ChannelName = "web"

// Adapter implements channels.ChannelAdapter for the web channel.
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
		return errors.New("web: nil gateway")
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

// Mount attaches /web/* to r. Must be called after Start.
func (a *Adapter) Mount(r chi.Router) {
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
	r.Route("/web", func(r chi.Router) {
		r.Get("/healthz", a.healthz)
		r.Post("/messages", a.postMessages)
		r.Get("/messages", a.getMessages)
	})
}

const maxInboundBytes = 4 << 20 // 4 MiB, attachments included.

type inboundRequest struct {
	ChannelID   string                 `json:"channelId"`
	UserID      string                 `json:"userId"`
	ThreadID    string                 `json:"threadId"`
	Text        string                 `json:"text"`
	Subject     string                 `json:"subject,omitempty"`
	ReplyTo     string                 `json:"replyTo,omitempty"`
	Attachments []attachmentDescriptor `json:"attachments,omitempty"`
}

type attachmentDescriptor struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	// Bytes is base64-encoded in the JSON payload; decoded by encoding/json.
	Bytes []byte `json:"bytes"`
}

func (a *Adapter) healthz(w http.ResponseWriter, _ *http.Request) {
	if !a.started.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok")
}

func (a *Adapter) postMessages(w http.ResponseWriter, r *http.Request) {
	if !a.started.Load() {
		http.Error(w, "web adapter not started", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxInboundBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(body)) > maxInboundBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds limit")
		return
	}

	var in inboundRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	if in.ChannelID == "" || in.UserID == "" || in.ThreadID == "" || in.Text == "" {
		writeJSONError(w, http.StatusBadRequest, "channelId, userId, threadId, text are all required")
		return
	}

	msg := channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: in.ChannelID,
		UserID:    in.UserID,
		ThreadID:  in.ThreadID,
		Text:      in.Text,
		ReplyTo:   in.ReplyTo,
		Subject:   in.Subject,
	}
	for _, att := range in.Attachments {
		msg.Attachments = append(msg.Attachments, channels.Attachment{
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Bytes:       att.Bytes,
		})
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
			fmt.Fprintf(w, "event: done\ndata: {}\n\n")
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

func (a *Adapter) getMessages(w http.ResponseWriter, r *http.Request) {
	if !a.started.Load() {
		http.Error(w, "web adapter not started", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	channelID := q.Get("channelId")
	userID := q.Get("userId")
	threadID := q.Get("threadId")
	if channelID == "" || userID == "" || threadID == "" {
		writeJSONError(w, http.StatusBadRequest, "channelId, userId, threadId are all required")
		return
	}

	ref, err := a.gw.Resolve(r.Context(), channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: channelID,
		UserID:    userID,
		ThreadID:  threadID,
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
		writeJSONError(w, http.StatusNotFound, "no instance bound to this thread and auto-create is disabled")
		return
	}
	a.Logger.Error("web: resolve failed", "error", err)
	writeJSONError(w, http.StatusBadGateway, "resolve: "+err.Error())
}

func setSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, err error) {
	fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
	flusher.Flush()
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": http.StatusText(code)},
	})
}
