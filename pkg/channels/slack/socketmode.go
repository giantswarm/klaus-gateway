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

	resp, err := http.DefaultClient.Do(req) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

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
	Event slackInnerEvent `json:"event"`
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
	defer ws.Close()

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

		if env.Type != "events_api" {
			continue
		}

		var payload smEventPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			continue
		}

		go c.handleEvent(ctx, payload.Event)
	}
}

func (c *socketModeClient) handleEvent(ctx context.Context, inner slackInnerEvent) {
	msg, ok := inner.toInboundMessage()
	if !ok {
		return
	}
	if err := c.adapter.dispatch(ctx, msg, inner.Channel, inner.TS); err != nil {
		c.logger.Error("slack socket mode: dispatch error", "channel", inner.Channel, "error", err)
	}
}
