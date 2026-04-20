// Package slack is the Slack channel adapter for klaus-gateway.
//
// Two connection modes are supported:
//   - events: Slack Events API HTTP webhook (production).
//   - socketmode: Slack Socket Mode WebSocket (development).
//
// The adapter is disabled by default; set --slack-enabled (or
// KLAUS_GATEWAY_SLACK_ENABLED=true) to activate it.
package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// ChannelName identifies the Slack adapter in routing keys.
const ChannelName = "slack"

// Mode constants for the Slack connection method.
const (
	ModeEvents     = "events"
	ModeSocketMode = "socketmode"
)

// Adapter implements channels.ChannelAdapter for the Slack channel.
type Adapter struct {
	Logger  *slog.Logger
	Mode    string
	Secrets Secrets
	// APIBase overrides the Slack Web API base URL. Empty uses the default
	// (https://slack.com/api). Set in tests to point at a fake server.
	APIBase string

	gw        channels.Gateway
	started   atomic.Bool
	evHandler http.Handler
}

// Name returns the channel name used in routing keys.
func (a *Adapter) Name() string { return ChannelName }

// Start wires the Gateway facade and initialises the chosen connection mode.
func (a *Adapter) Start(ctx context.Context, gw channels.Gateway) error {
	if gw == nil {
		return errors.New("slack: nil gateway")
	}
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
	a.gw = gw

	switch a.Mode {
	case ModeEvents, "":
		a.evHandler = &eventsHandler{
			signingSecret: a.Secrets.SigningSecret,
			botToken:      a.Secrets.BotToken,
			adapter:       a,
			logger:        a.Logger,
			ctx:           ctx,
		}
	case ModeSocketMode:
		if a.Secrets.AppToken == "" {
			return errors.New("slack: app_token is required in socketmode")
		}
		sm := &socketModeClient{
			appToken: a.Secrets.AppToken,
			botToken: a.Secrets.BotToken,
			adapter:  a,
			logger:   a.Logger,
		}
		go sm.run(ctx)
	default:
		return fmt.Errorf("slack: unknown mode %q: want %q or %q", a.Mode, ModeEvents, ModeSocketMode)
	}

	a.started.Store(true)
	return nil
}

// Stop marks the adapter as stopped. The context passed to Start is the
// primary shutdown mechanism for background goroutines.
func (a *Adapter) Stop(_ context.Context) error {
	a.started.Store(false)
	return nil
}

// Mount attaches /channels/slack/events to r. No-op in socketmode.
func (a *Adapter) Mount(r chi.Router) {
	if a.evHandler == nil {
		return
	}
	r.Route("/channels/slack", func(r chi.Router) {
		r.Handle("/events", a.evHandler)
	})
}

// apiClient returns a Slack Web API client using the adapter's bot token
// and the optional test-override base URL.
func (a *Adapter) apiClient() *slackAPIClient {
	base := a.APIBase
	if base == "" {
		base = slackAPIBase
	}
	return &slackAPIClient{botToken: a.Secrets.BotToken, baseURL: base}
}

// dispatch resolves an inbound Slack message to a Klaus instance, posts a
// placeholder reply, and streams the completion back via chat.update batches.
func (a *Adapter) dispatch(ctx context.Context, msg channels.InboundMessage, slackChannel, replyTS string) error {
	if !a.started.Load() {
		return errors.New("slack: adapter not started")
	}

	ref, err := a.gw.Resolve(ctx, msg)
	if err != nil {
		return fmt.Errorf("slack: resolve: %w", err)
	}

	client := a.apiClient()
	ts, err := client.postMessage(ctx, slackChannel, "_thinking\u2026_")
	if err != nil {
		return fmt.Errorf("slack: post placeholder: %w", err)
	}
	_ = replyTS // kept for future thread-reply support

	deltas, err := a.gw.SendCompletion(ctx, ref, msg)
	if err != nil {
		return fmt.Errorf("slack: send completion: %w", err)
	}

	w := newBatchedWriterWithClient(client, slackChannel, ts)
	return w.run(ctx, deltas)
}

// slackInnerEvent is the inner event object present in both Events API
// and Socket Mode payloads.
type slackInnerEvent struct {
	Type     string `json:"type"`
	SubType  string `json:"subtype,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	User     string `json:"user"`
	Text     string `json:"text"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
}

// toInboundMessage maps a Slack inner event to the normalised InboundMessage.
// Returns false when the event should be ignored (bot message, empty text, …).
func (e slackInnerEvent) toInboundMessage() (channels.InboundMessage, bool) {
	if e.BotID != "" || e.SubType != "" {
		return channels.InboundMessage{}, false
	}
	switch e.Type {
	case "app_mention", "message":
	default:
		return channels.InboundMessage{}, false
	}
	threadID := e.ThreadTS
	if threadID == "" {
		threadID = e.TS
	}
	text := StripMention(e.Text)
	if text == "" {
		return channels.InboundMessage{}, false
	}
	return channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: e.Channel,
		UserID:    e.User,
		ThreadID:  threadID,
		Text:      text,
	}, true
}

// StripMention removes leading <@USERID> tokens that Slack injects into
// app_mention event text.
func StripMention(text string) string {
	s := text
	for len(s) > 0 && s[0] == '<' {
		end := 0
		for end < len(s) && s[end] != '>' {
			end++
		}
		if end >= len(s) {
			break
		}
		s = s[end+1:]
		// Consume optional trailing space.
		if len(s) > 0 && s[0] == ' ' {
			s = s[1:]
		}
	}
	return s
}
