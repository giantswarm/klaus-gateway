package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	// slackMaxMessageLen caps a single Slack message body, under the 40 000-char
	// hard limit with headroom for mrkdwn expansion. A reply that exceeds it is
	// rolled over into follow-up in-thread messages by flush.
	slackMaxMessageLen = 39000
)

// batchedWriter accumulates OutboundDelta content and periodically calls
// chat.update to stay within Slack's rate limits (~4 updates/sec/channel).
//
// Two Slack messages are managed:
//   - ts (the main reply): accumulates DeltaText content.
//
// When the stream ends on a DeltaPrompt, run() captures it in promptDelta
// (flushing the main buffer first) and returns nil. The caller is responsible
// for posting the approval prompt and registering the pending task.
type batchedWriter struct {
	client   *slackAPIClient
	channel  string
	threadTS string // thread root — used when posting the status message
	ts       string // main reply message timestamp
	logger   *slog.Logger

	mu          sync.Mutex
	buf         strings.Builder
	flushedLen  int                     // length of buf at the last chat.update; skips no-op flushes
	promptDelta *channels.OutboundDelta // set when stream ends on DeltaPrompt
	// tailTS holds the timestamps of overflow messages posted when the reply
	// outgrows a single Slack message. Only touched from run()'s goroutine.
	tailTS []string
}

func newBatchedWriterWithClient(client *slackAPIClient, channel, ts, threadTS string, logger *slog.Logger) *batchedWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &batchedWriter{
		client:   client,
		channel:  channel,
		ts:       ts,
		threadTS: threadTS,
		logger:   logger,
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
			switch d.Kind {
			case channels.DeltaText:
				if d.Content == "" {
					continue
				}
				w.mu.Lock()
				w.buf.WriteString(d.Content)
				w.mu.Unlock()
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

func (w *batchedWriter) flush(ctx context.Context) error {
	w.mu.Lock()
	// Skip the chat.update when nothing new accumulated since the last flush;
	// the ticker fires every batchInterval whether or not content changed.
	if w.buf.Len() == w.flushedLen {
		w.mu.Unlock()
		return nil
	}
	text := w.buf.String()
	w.flushedLen = w.buf.Len()
	w.mu.Unlock()

	// A reply that fits one message is a single chat.update of the main message.
	// A larger reply rolls over: the head updates the main message and each
	// subsequent chunk updates (or, the first time, posts) a stable follow-up
	// message in-thread, so growing replies extend in place without duplicating.
	//
	// ponytail: a multi-chunk reply makes one API call per chunk every
	// batchInterval, so a reply spanning N messages costs N calls/flush against
	// Slack's ~4 updates/sec/channel. Fine while >39 KB replies are rare; revisit
	// with per-call pacing if they become common.
	chunks := splitAtLines(markdownToMrkdwn(text), slackMaxMessageLen)
	if err := w.client.chatUpdate(ctx, w.channel, w.ts, chunks[0]); err != nil {
		return err
	}
	for i, chunk := range chunks[1:] {
		if i < len(w.tailTS) {
			if err := w.client.chatUpdate(ctx, w.channel, w.tailTS[i], chunk); err != nil {
				return err
			}
			continue
		}
		ts, err := w.client.postMessage(ctx, w.channel, chunk, w.threadTS)
		if err != nil {
			return err
		}
		w.tailTS = append(w.tailTS, ts)
	}
	return nil
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
			bkValue:    encodeChoiceValue(threadID, i),
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

// truncateButtonLabel keeps a button label within Slack's 75-character limit,
// counting runes (not bytes) so a multi-byte glyph is never split mid-rune.
func truncateButtonLabel(s string) string {
	const max = 75
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
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
