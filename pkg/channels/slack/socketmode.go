package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/net/websocket"
)

const (
	appsConnectionsOpen = "https://slack.com/api/apps.connections.open"
	smReconnectDelay    = 5 * time.Second
	smTypeHello         = "hello"
)

// socketModeClient connects to Slack Socket Mode and forwards events to the
// adapter. Intended for development environments; use Events API in production.
type socketModeClient struct {
	appToken string
	botToken string
	adapter  *Adapter
	logger   *slog.Logger
}

// run connects and reconnects until ctx is cancelled.
func (c *socketModeClient) run(ctx context.Context) {
	for {
		if err := c.connect(ctx); err != nil && ctx.Err() == nil {
			c.logger.Error("slack socket mode: connection error, will retry",
				"delay", smReconnectDelay, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(smReconnectDelay):
		}
	}
}

// openWSURL calls apps.connections.open and returns the wss:// URL.
func (c *socketModeClient) openWSURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, appsConnectionsOpen, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.appToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := slackHTTPClient.Do(req) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("apps.connections.open: %s", result.Error)
	}
	return result.URL, nil
}

type smEnvelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}

type smEventPayload struct {
	EventID string          `json:"event_id"`
	Event   slackInnerEvent `json:"event"`
}

// connect dials the Socket Mode WebSocket and handles events until the
// connection drops or ctx is cancelled.
func (c *socketModeClient) connect(ctx context.Context) error {
	wsURL, err := c.openWSURL(ctx)
	if err != nil {
		return fmt.Errorf("open WS URL: %w", err)
	}

	// golang.org/x/net/websocket is deprecated but already available as an
	// indirect dependency; it is adequate for this development-mode feature.
	ws, err := websocket.Dial(wsURL, "", "https://slack.com") //nolint:staticcheck
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = ws.Close() }()

	c.logger.Info("slack socket mode: connected")

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.readLoop(ctx, ws)
	}()

	select {
	case <-ctx.Done():
		_ = ws.Close()
	case <-done:
	}
	return nil
}

func (c *socketModeClient) readLoop(ctx context.Context, ws *websocket.Conn) {
	for {
		var raw []byte
		if err := websocket.Message.Receive(ws, &raw); err != nil { //nolint:staticcheck
			if err != io.EOF && ctx.Err() == nil {
				c.logger.Warn("slack socket mode: receive error", "error", err)
			}
			return
		}

		var env smEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}

		// Acknowledge the envelope before any further processing.
		if env.EnvelopeID != "" {
			ack := `{"envelope_id":"` + env.EnvelopeID + `"}`
			_ = websocket.Message.Send(ws, ack) //nolint:staticcheck
		}

		// Slack load-balances events across every open Socket Mode connection
		// for an app, delivering each to exactly one of them. A second consumer
		// (a deployed gateway, another developer's session) silently steals a
		// share of messages. Surface num_connections on connect so a "nothing
		// arrives" symptom is diagnosable instead of baffling.
		if env.Type == smTypeHello {
			var h struct {
				NumConnections int `json:"num_connections"`
			}
			_ = json.Unmarshal(raw, &h)
			if h.NumConnections > 1 {
				c.logger.Warn("slack socket mode: multiple active connections for this app; "+
					"Slack delivers each event to only one, so this gateway will miss messages "+
					"handled by the others. Stop the other consumer or use a dedicated app token.",
					"num_connections", h.NumConnections)
			} else {
				c.logger.Info("slack socket mode: sole active connection", "num_connections", h.NumConnections)
			}
			continue
		}

		// Block Kit button clicks (HITL choice/approve/deny) arrive as
		// interactive envelopes, not events_api. The HTTP /interactions
		// endpoint is only mounted in Events API mode, so Socket Mode must
		// route these itself or the rendered buttons would be inert.
		if env.Type == "interactive" {
			var payload interactionPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				continue
			}
			c.adapter.background(func(ctx context.Context) { c.adapter.routeInteraction(ctx, payload) })
			continue
		}

		if env.Type != "events_api" {
			continue
		}

		var payload smEventPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			continue
		}

		c.adapter.background(func(ctx context.Context) { c.adapter.handleInbound(ctx, payload.Event, payload.EventID) })
	}
}
