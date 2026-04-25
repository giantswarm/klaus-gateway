package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

const (
	batchInterval = 250 * time.Millisecond
	slackAPIBase  = "https://slack.com/api"
)

// batchedWriter accumulates OutboundDelta content and periodically calls
// chat.update to stay within Slack's rate limits (~4 updates/sec/channel).
type batchedWriter struct {
	client  *slackAPIClient
	channel string
	ts      string // Slack message timestamp (acts as the message ID for updates).

	mu  sync.Mutex
	buf strings.Builder
}

func newBatchedWriterWithClient(client *slackAPIClient, channel, ts string) *batchedWriter {
	return &batchedWriter{
		client:  client,
		channel: channel,
		ts:      ts,
	}
}

// run drains deltas from ch, batching chat.update calls at batchInterval.
func (w *batchedWriter) run(ctx context.Context, ch <-chan channels.OutboundDelta) error {
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case d, ok := <-ch:
			if !ok {
				return w.flush(ctx)
			}
			if d.Err != nil {
				return d.Err
			}
			if d.Done {
				return w.flush(ctx)
			}
			if d.Content == "" {
				continue
			}
			w.mu.Lock()
			w.buf.WriteString(d.Content)
			w.mu.Unlock()

		case <-ticker.C:
			if err := w.flush(ctx); err != nil {
				return err
			}
		}
	}
}

func (w *batchedWriter) flush(ctx context.Context) error {
	w.mu.Lock()
	text := w.buf.String()
	w.mu.Unlock()
	if text == "" {
		return nil
	}
	return w.client.chatUpdate(ctx, w.channel, w.ts, text)
}

// slackAPIClient is a minimal HTTP client for the Slack Web API.
type slackAPIClient struct {
	botToken string
	baseURL  string
}

func (c *slackAPIClient) postMessage(ctx context.Context, channel, text string) (string, error) {
	params := url.Values{
		"channel": {channel},
		"text":    {text},
	}
	return c.post(ctx, "chat.postMessage", params)
}

func (c *slackAPIClient) chatUpdate(ctx context.Context, channel, ts, text string) error {
	params := url.Values{
		"channel": {channel},
		"ts":      {ts},
		"text":    {text},
	}
	_, err := c.post(ctx, "chat.update", params)
	return err
}

type slackResponse struct {
	OK    bool   `json:"ok"`
	Ts    string `json:"ts"`
	Error string `json:"error,omitempty"`
}

func (c *slackAPIClient) post(ctx context.Context, method string, params url.Values) (string, error) {
	target := c.baseURL + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(params.Encode()))
	if err != nil {
		return "", fmt.Errorf("slack %s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := http.DefaultClient.Do(req) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("slack %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result slackResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("slack %s: decode response: %w", method, err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack %s: %s", method, result.Error)
	}
	return result.Ts, nil
}
