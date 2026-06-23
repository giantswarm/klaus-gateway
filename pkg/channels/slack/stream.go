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
//
// Two Slack messages are managed:
//   - ts (the main reply): accumulates DeltaText content.
//   - statusTS (the status line, posted lazily): shows the latest
//     DeltaThinking or DeltaTool text. Updated in-place rather than
//     accumulating; overwritten on each new status delta.
//
// When the stream ends on a DeltaPrompt, run() captures it in promptDelta
// (flushing the main buffer first) and returns nil. The caller is responsible
// for posting the approval prompt and registering the pending task.
type batchedWriter struct {
	client   *slackAPIClient
	channel  string
	threadTS string // thread root — used when posting the status message
	ts       string // main reply message timestamp

	mu          sync.Mutex
	buf         strings.Builder
	statusTS    string                  // timestamp of the lazily-created status message; empty until first status delta
	promptDelta *channels.OutboundDelta // set when stream ends on DeltaPrompt
}

func newBatchedWriterWithClient(client *slackAPIClient, channel, ts, threadTS string) *batchedWriter {
	return &batchedWriter{
		client:   client,
		channel:  channel,
		ts:       ts,
		threadTS: threadTS,
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
			if d.Content == "" && d.Tool == "" {
				continue
			}
			switch d.Kind {
			case channels.DeltaText:
				w.mu.Lock()
				w.buf.WriteString(d.Content)
				w.mu.Unlock()
			case channels.DeltaThinking:
				if err := w.updateStatus(ctx, "_"+d.Content+"_"); err != nil {
					return err
				}
			case channels.DeltaTool:
				if err := w.updateStatus(ctx, formatToolStatus(d)); err != nil {
					return err
				}
			case channels.DeltaPrompt:
				// Flush partial text so far, then hand off to the caller to post
				// the interactive approval prompt.
				if err := w.flush(ctx); err != nil {
					return err
				}
				w.mu.Lock()
				w.promptDelta = &d
				w.mu.Unlock()
				return nil
			}

		case <-ticker.C:
			if err := w.flush(ctx); err != nil {
				return err
			}
		}
	}
}

// formatToolStatus returns the mrkdwn status line for a tool delta.
func formatToolStatus(d channels.OutboundDelta) string {
	name := d.Tool
	if name == "" {
		name = d.Content
	}
	switch d.State {
	case channels.ToolDone:
		return "✅ _" + name + "_"
	case channels.ToolError:
		return "❌ _" + name + "_"
	default:
		return "⏳ _" + name + "…_"
	}
}

// updateStatus posts or updates the status line message in-thread.
func (w *batchedWriter) updateStatus(ctx context.Context, text string) error {
	w.mu.Lock()
	statusTS := w.statusTS
	w.mu.Unlock()

	if statusTS == "" {
		ts, err := w.client.postMessage(ctx, w.channel, text, w.threadTS)
		if err != nil {
			return err
		}
		w.mu.Lock()
		w.statusTS = ts
		w.mu.Unlock()
		return nil
	}
	return w.client.chatUpdate(ctx, w.channel, statusTS, text)
}

func (w *batchedWriter) flush(ctx context.Context) error {
	w.mu.Lock()
	text := w.buf.String()
	w.mu.Unlock()
	if text == "" {
		return nil
	}
	return w.client.chatUpdate(ctx, w.channel, w.ts, markdownToMrkdwn(text))
}

// slackAPIClient is a minimal HTTP client for the Slack Web API.
type slackAPIClient struct {
	botToken string
	baseURL  string
}

func (c *slackAPIClient) postMessage(ctx context.Context, channel, text, threadTS string) (string, error) {
	params := url.Values{
		paramChannel: {channel},
		paramText:    {text},
	}
	if threadTS != "" {
		params.Set(paramThreadTS, threadTS)
	}
	return c.post(ctx, "chat.postMessage", params)
}

// lookupUserEmail returns the email from the user's Slack profile.
// Falls back to the raw Slack user ID on any error so dispatch is never blocked.
func (c *slackAPIClient) lookupUserEmail(ctx context.Context, userID string) (string, error) {
	params := url.Values{paramUser: {userID}}
	target := c.baseURL + "/users.info?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("slack users.info: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := http.DefaultClient.Do(req) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("slack users.info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		OK   bool   `json:"ok"`
		Err  string `json:"error,omitempty"`
		User struct {
			Profile struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("slack users.info: decode: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack users.info: %s", result.Err)
	}
	return result.User.Profile.Email, nil
}

func (c *slackAPIClient) chatUpdate(ctx context.Context, channel, ts, text string) error {
	params := url.Values{
		paramChannel: {channel},
		paramTS:      {ts},
		paramText:    {text},
	}
	_, err := c.post(ctx, "chat.update", params)
	return err
}

// postApprovalPrompt posts a Block Kit message with ✅/❌ buttons for HITL
// approval. The button values encode the threadID so the interaction handler
// can route the response back.
func (c *slackAPIClient) postApprovalPrompt(ctx context.Context, channel, threadID, promptText, taskID string) error {
	text := "_Waiting for approval…_"
	if promptText != "" {
		text = promptText
	}
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     text,
		paramBlocks: []any{
			map[string]any{
				bkType: bkSection,
				bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
			},
			map[string]any{
				bkType: bkActions,
				bkElements: []any{
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "✅ Approve"},
						bkStyle:    bkPrimary,
						bkActionID: hitlApprove,
						bkValue:    threadID,
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "❌ Deny"},
						bkStyle:    bkDanger,
						bkActionID: hitlDeny,
						bkValue:    threadID,
					},
				},
			},
		},
	}
	_, err := c.postJSON(ctx, "chat.postMessage", body)
	return err
}

// postChoicePrompt posts an ask_user question with one Block Kit button per
// choice. Each button's value encodes the threadID, question index, and choice
// index so the interaction handler can resolve the selected answer label.
func (c *slackAPIClient) postChoicePrompt(ctx context.Context, channel, threadID, question string, choices []string) error {
	elements := make([]any, 0, len(choices))
	for i, choice := range choices {
		elements = append(elements, map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: truncateButtonLabel(choice)},
			bkActionID: fmt.Sprintf("%s_%d", hitlChoice, i),
			bkValue:    encodeChoiceValue(threadID, 0, i),
		})
	}
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     question,
		paramBlocks: []any{
			map[string]any{
				bkType: bkSection,
				bkText: map[string]any{bkType: bkMrkdwn, bkText: "*" + question + "*"},
			},
			map[string]any{
				bkType:     bkActions,
				bkElements: elements,
			},
		},
	}
	_, err := c.postJSON(ctx, "chat.postMessage", body)
	return err
}

// postSignInPrompt posts an ephemeral (visible only to the target user)
// Block Kit message with a "Sign in to Giant Swarm" button linking to linkURL.
// It is used to nudge an unlinked Slack user into the OBO account-linking flow.
// When threadID is set the prompt is posted in-thread.
func (c *slackAPIClient) postSignInPrompt(ctx context.Context, channel, threadID, user, linkURL string) error {
	const text = "Sign in to Giant Swarm so I can act on your behalf. " +
		"Until you do, I run as the gateway service account."
	body := map[string]any{
		paramChannel: channel,
		paramUser:    user,
		paramText:    text,
		paramBlocks: []any{
			map[string]any{
				bkType: bkSection,
				bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
			},
			map[string]any{
				bkType: bkActions,
				bkElements: []any{
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "Sign in to Giant Swarm"},
						bkStyle:    bkPrimary,
						bkActionID: oboSignIn,
						bkURL:      linkURL,
					},
				},
			},
		},
	}
	if threadID != "" {
		body[paramThreadTS] = threadID
	}
	_, err := c.postJSON(ctx, "chat.postEphemeral", body)
	return err
}

// truncateButtonLabel keeps a button label within Slack's 75-char limit.
func truncateButtonLabel(s string) string {
	const max = 75
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// chatUpdateBlocks replaces a Block Kit message with plain text (used to mark
// an approval decision after the user clicks a button).
func (c *slackAPIClient) chatUpdateBlocks(ctx context.Context, channel, ts, text string) error {
	body := map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramText:    text,
		paramBlocks:  []any{},
	}
	_, err := c.postJSON(ctx, "chat.update", body)
	return err
}

func (c *slackAPIClient) postJSON(ctx context.Context, method string, body any) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("slack %s: marshal: %w", method, err)
	}
	target := c.baseURL + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(data)))
	if err != nil {
		return "", fmt.Errorf("slack %s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
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
