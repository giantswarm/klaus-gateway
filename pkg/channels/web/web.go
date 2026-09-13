// Package web is the HTTP channel adapter that browser front-ends and
// headless drivers (the lab's proofs) call into.
//
// Surface:
//
//	POST /web/messages     -- send one user message (or a HITL decision), receive deltas as SSE
//	GET  /web/messages     -- fetch history for (channelId, userId, threadId)
//	GET  /web/agents       -- list the agents a message may name
//	GET  /web/healthz      -- 200 once Start has run
//
// The adapter is channel-agnostic on the wire: it normalises requests into
// channels.InboundMessage and hands off to the Gateway facade. The caller's
// bearer token is forwarded as the identity of the turn.
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

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing"
)

// ChannelName identifies the web adapter in routing keys.
const ChannelName = "web"

// Adapter implements channels.ChannelAdapter for the web channel.
type Adapter struct {
	Logger       *slog.Logger
	DefaultAgent string

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
		r.Get("/agents", a.getAgents)
	})
}

// agentLister is the optional Gateway capability that lists the selectable
// agents. The Facade implements it when a kagent client is configured.
type agentLister interface {
	ListAgents(ctx context.Context) ([]pkga2a.AgentInfo, error)
}

const maxInboundBytes = 4 << 20 // 4 MiB, attachments included.

type inboundRequest struct {
	ChannelID   string                 `json:"channelId"`
	UserID      string                 `json:"userId"`
	ThreadID    string                 `json:"threadId"`
	Text        string                 `json:"text"`
	AgentRef    string                 `json:"agentRef,omitempty"`
	Subject     string                 `json:"subject,omitempty"`
	ReplyTo     string                 `json:"replyTo,omitempty"`
	Attachments []attachmentDescriptor `json:"attachments,omitempty"`
	// TaskID resumes the task a previous turn paused on (the taskId of its
	// prompt event) instead of starting a new one.
	TaskID string `json:"taskId,omitempty"`
	// Decision answers the prompt of TaskID: "approve" or "reject" for a tool
	// approval (rejectionReason optional), the positional askUserAnswers for an
	// ask_user question. Text is kept as the decision's readable label.
	Decision *decisionDescriptor `json:"decision,omitempty"`
}

type decisionDescriptor struct {
	Type            string     `json:"type"`
	AskUserAnswers  [][]string `json:"askUserAnswers,omitempty"`
	RejectionReason string     `json:"rejectionReason,omitempty"`
}

// promptEvent is the SSE payload of a turn paused on a prompt: the task to
// resume with a decision and what is being asked.
type promptEvent struct {
	TaskID string            `json:"taskId"`
	Text   string            `json:"text"`
	Prompt *promptDescriptor `json:"prompt,omitempty"`
}

type promptDescriptor struct {
	ToolName  string                  `json:"toolName"`
	Hint      string                  `json:"hint,omitempty"`
	Tools     []promptTool            `json:"tools,omitempty"`
	Questions []channels.HitlQuestion `json:"questions,omitempty"`
}

type promptTool struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

func describePrompt(p *channels.HitlPrompt) *promptDescriptor {
	if p == nil {
		return nil
	}
	d := &promptDescriptor{ToolName: p.ToolName, Hint: p.Hint, Questions: p.Questions}
	for _, t := range p.Tools {
		d.Tools = append(d.Tools, promptTool{ID: t.ID, Name: t.Name, Args: t.Args})
	}
	return d
}

// agentDescriptor is one entry of GET /web/agents.
type agentDescriptor struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	DisplayName string `json:"displayName,omitempty"`
	IconURL     string `json:"iconUrl,omitempty"`
	Description string `json:"description,omitempty"`
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
	if in.ChannelID == "" || in.UserID == "" || in.ThreadID == "" || (in.Text == "" && in.Decision == nil) {
		writeJSONError(w, http.StatusBadRequest, "channelId, userId, threadId, text are all required")
		return
	}
	if in.Decision != nil && in.TaskID == "" {
		writeJSONError(w, http.StatusBadRequest, "a decision needs the taskId of the prompt it answers")
		return
	}

	msg := channels.InboundMessage{
		Channel:     ChannelName,
		ChannelID:   in.ChannelID,
		UserID:      in.UserID,
		ThreadID:    in.ThreadID,
		Text:        in.Text,
		ReplyTo:     in.ReplyTo,
		Subject:     in.Subject,
		BearerToken: channels.BearerToken(r),
		TaskID:      in.TaskID,
	}
	if in.Decision != nil {
		msg.Decision = &channels.HitlDecision{Type: in.Decision.Type, AskUserAnswers: in.Decision.AskUserAnswers, RejectionReason: in.Decision.RejectionReason}
	}
	if in.AgentRef != "" {
		msg.AgentRef = in.AgentRef
	} else if a.DefaultAgent != "" {
		msg.AgentRef = a.DefaultAgent
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
			_, _ = fmt.Fprintf(w, "event: done\ndata: {}\n\n")
			flusher.Flush()
			continue
		}
		if d.Kind == channels.DeltaPrompt {
			// The turn pauses here; the client resumes it with a decision on
			// the task. Nothing else follows on this stream.
			_, _ = io.WriteString(w, "event: prompt\ndata: ")
			_ = enc.Encode(promptEvent{TaskID: d.TaskID, Text: d.Content, Prompt: describePrompt(d.Prompt)})
			_, _ = io.WriteString(w, "\n")
			flusher.Flush()
			continue
		}
		if d.Content == "" {
			continue
		}
		if _, err := io.WriteString(w, "data: "); err != nil {
			return
		}
		if err := enc.Encode(map[string]string{"content": d.StreamText()}); err != nil {
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

// getAgents lists the agents a message may name, read from the kagent
// controller as the caller (the bearer token is forwarded).
func (a *Adapter) getAgents(w http.ResponseWriter, r *http.Request) {
	if !a.started.Load() {
		http.Error(w, "web adapter not started", http.StatusServiceUnavailable)
		return
	}
	lister, ok := a.gw.(agentLister)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "agent discovery is not configured on this gateway")
		return
	}
	ctx := pkga2a.WithChannel(r.Context(), ChannelName)
	ctx = pkga2a.WithForwardedToken(ctx, channels.BearerToken(r))
	agents, err := lister.ListAgents(ctx)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "list agents: "+err.Error())
		return
	}
	out := make([]agentDescriptor, 0, len(agents))
	for _, ag := range agents {
		out = append(out, agentDescriptor{Name: ag.Name, Namespace: ag.Namespace, DisplayName: ag.DisplayName, IconURL: ag.IconURL, Description: ag.Description})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"agents": out})
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
