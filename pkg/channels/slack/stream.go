package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

const (
	batchInterval = 250 * time.Millisecond
	slackAPIBase  = "https://slack.com/api"
	// slackMarkdownBlockMax caps the text of one Block Kit markdown block,
	// Slack's 12 000-char limit. splitMarkdown budgets the fence auto-close and
	// reopen inside this cap, so emitted chunks never exceed it.
	slackMarkdownBlockMax = 12000
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
	details  detailsLevel // tool-activity verbosity snapshotted at turn start

	// turnUsage accumulates the per-LLM-call usage kagent reports across the
	// turn into the turn total. Only touched from run()'s goroutine.
	turnUsage channels.TurnUsage
	// toolsRendered counts tool-activity messages posted this turn so a tool-heavy
	// turn does not flood the thread (or hit Slack post rate limits). Only touched
	// from run()'s goroutine.
	toolsRendered int

	mu          sync.Mutex
	buf         strings.Builder
	flushedLen  int                     // length of buf at the last chat.update; skips no-op flushes
	promptDelta *channels.OutboundDelta // set when stream ends on DeltaPrompt
	// tailTS holds the timestamps of overflow messages posted when the reply
	// outgrows a single Slack message. Only touched from run()'s goroutine.
	tailTS []string
}

func newBatchedWriterWithClient(client *slackAPIClient, channel, ts, threadTS string, details detailsLevel, logger *slog.Logger) *batchedWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &batchedWriter{
		client:   client,
		channel:  channel,
		ts:       ts,
		threadTS: threadTS,
		details:  details,
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
			if d.Usage != nil {
				// kagent reports usage per LLM call, so sum across the turn for
				// the turn total (the terminal event alone under-counts).
				w.turnUsage.InputTokens += d.Usage.InputTokens
				w.turnUsage.OutputTokens += d.Usage.OutputTokens
				w.turnUsage.TotalTokens += d.Usage.TotalTokens
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
			case channels.DeltaToolActivity:
				w.renderToolActivity(ctx, d.Tool)
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

// tool-activity rendering caps, kept compact so a default-on stream does not
// overwhelm the thread; Slack additionally collapses long code blocks.
const (
	toolArgsMax   = 500
	toolResultMax = 800
	// maxToolMessages bounds tool-activity posts per turn. Past it, one
	// truncation note is posted and the rest are silent, so a turn with many
	// tool calls does not flood the thread or hit Slack post rate limits.
	maxToolMessages = 10
)

// renderToolActivity posts a compact record of a tool call (and, at
// detailsFull, its result) when details are enabled. Rendered as a fenced code
// block so Slack collapses long payloads behind "show more". Capped per turn.
// Best-effort: a post failure is logged and never aborts the turn.
func (w *batchedWriter) renderToolActivity(ctx context.Context, tool *channels.ToolActivity) {
	if w.details == detailsOff || tool == nil {
		return
	}
	var md string
	switch tool.Kind {
	case channels.ToolCall:
		md = "🔧 `" + tool.Name + "`"
		if summary := compactJSON(tool.Args, toolArgsMax); summary != "" {
			md += "\n```\n" + summary + "\n```"
		}
	case channels.ToolResult:
		if w.details != detailsFull {
			return
		}
		preview := compactJSON(tool.Response, toolResultMax)
		if preview == "" {
			return
		}
		md = "↳ `" + tool.Name + "` result\n```\n" + preview + "\n```"
	default:
		return
	}

	w.toolsRendered++
	switch {
	case w.toolsRendered > maxToolMessages+1:
		return // already posted the truncation note
	case w.toolsRendered == maxToolMessages+1:
		md = "_…further tool activity hidden this turn (`/details off` to quiet)._"
	}

	if _, err := w.client.postMarkdown(ctx, w.channel, md, w.threadTS); err != nil {
		w.logger.Warn("slack: post tool activity failed", "tool", tool.Name, "error", err)
	}
}

// compactJSON marshals a tool payload to a single line, truncated to max runes.
// Returns "" for an empty or unmarshalable payload.
func compactJSON(v map[string]any, max int) string {
	if len(v) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	rs := []rune(string(b))
	if len(rs) > max {
		return string(rs[:max]) + "…"
	}
	return string(rs)
}

// wroteContent reports whether any agent text has been flushed to Slack. Used
// after the run loop to detect a turn that produced no output.
func (w *batchedWriter) wroteContent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushedLen > 0
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
	flushingLen := w.buf.Len()
	w.mu.Unlock()

	// Agent output renders as Block Kit markdown blocks. A reply that
	// fits one block updates the main message; a larger reply rolls over into
	// stable follow-up messages in-thread. The head message is posted lazily on
	// the first flush when ts is empty (reactions mode), else updated in place.
	//
	// ponytail: a multi-chunk reply makes one API call per chunk every
	// batchInterval, so a reply spanning N messages costs N calls/flush against
	// Slack's ~4 updates/sec/channel. Fine while >12 KB replies are rare; revisit
	// with per-call pacing if they become common.
	chunks := splitMarkdown(text, slackMarkdownBlockMax)
	if w.ts == "" {
		ts, err := w.client.postMarkdown(ctx, w.channel, chunks[0], w.threadTS)
		if err != nil {
			return err
		}
		w.ts = ts
	} else if err := w.client.chatUpdateMarkdown(ctx, w.channel, w.ts, chunks[0]); err != nil {
		return err
	}
	for i, chunk := range chunks[1:] {
		if i < len(w.tailTS) {
			if err := w.client.chatUpdateMarkdown(ctx, w.channel, w.tailTS[i], chunk); err != nil {
				return err
			}
			continue
		}
		ts, err := w.client.postMarkdown(ctx, w.channel, chunk, w.threadTS)
		if err != nil {
			return err
		}
		w.tailTS = append(w.tailTS, ts)
	}
	// flushedLen advances only once every chunk landed, so a failed flush leaves
	// the delta pending and a retried flush re-sends it (chat.update on the head
	// and already-posted tails is idempotent).
	w.mu.Lock()
	if flushingLen > w.flushedLen {
		w.flushedLen = flushingLen
	}
	w.mu.Unlock()
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
// users.info is Tier-4 rate-limited, so the call goes through the same
// 429-retrying transport as every other Web API call.
func (c *slackAPIClient) lookupUserEmail(ctx context.Context, userID string) (string, error) {
	params := url.Values{paramUser: {userID}}
	body, err := c.call(ctx, "users.info", "application/x-www-form-urlencoded", params.Encode())
	if err != nil {
		return "", err
	}

	var result struct {
		OK   bool   `json:"ok"`
		Err  string `json:"error,omitempty"`
		User struct {
			Profile struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack users.info: decode: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack users.info: %s", result.Err)
	}
	return result.User.Profile.Email, nil
}

// errReactionsUnsupported reports that the bot cannot manage reactions (the
// reactions:write scope is missing, or the token type disallows it), so the
// caller should fall back to text-based progress.
var errReactionsUnsupported = errors.New("slack: reactions unsupported")

func (c *slackAPIClient) reactionsAdd(ctx context.Context, channel, ts, name string) error {
	return c.reaction(ctx, "reactions.add", channel, ts, name)
}

func (c *slackAPIClient) reactionsRemove(ctx context.Context, channel, ts, name string) error {
	return c.reaction(ctx, "reactions.remove", channel, ts, name)
}

func (c *slackAPIClient) reaction(ctx context.Context, method, channel, ts, name string) error {
	_, err := c.post(ctx, method, url.Values{
		paramChannel:   {channel},
		paramTimestamp: {ts},
		paramName:      {name},
	})
	if err != nil && (strings.Contains(err.Error(), "missing_scope") ||
		strings.Contains(err.Error(), "not_allowed_token_type")) {
		return errReactionsUnsupported
	}
	return err
}

// markdownBlocks wraps text in a single Block Kit markdown block, which renders
// Slack's supported Markdown (bold, italic, lists, tables, code blocks, ...)
// natively, without the mrkdwn conversion.
func markdownBlocks(md string) []any {
	return []any{map[string]any{bkType: bkMarkdown, bkText: md}}
}

// postMarkdown posts a new in-thread message rendered as a markdown block. The
// top-level text is the notification/accessibility fallback.
func (c *slackAPIClient) postMarkdown(ctx context.Context, channel, md, threadTS string) (string, error) {
	body := map[string]any{
		paramChannel: channel,
		paramText:    md,
		paramBlocks:  markdownBlocks(md),
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	return c.postJSON(ctx, "chat.postMessage", body)
}

// chatUpdateMarkdown replaces a message's content with a markdown block.
func (c *slackAPIClient) chatUpdateMarkdown(ctx context.Context, channel, ts, md string) error {
	body := map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramText:    md,
		paramBlocks:  markdownBlocks(md),
	}
	_, err := c.postJSON(ctx, "chat.update", body)
	return err
}

// postApprovalPrompt posts a Block Kit message with ✅/❌ buttons for HITL
// approval. The button values encode the threadID so the interaction handler
// can route the response back.
func (c *slackAPIClient) postApprovalPrompt(ctx context.Context, channel, threadID, promptText string) error {
	text := "_Waiting for approval…_"
	if promptText != "" {
		// promptText is agent-rendered (tool name, args, hint) and enters an
		// mrkdwn section block.
		text = escapeMrkdwn(promptText)
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
	// The question is agent-authored and enters an mrkdwn section block; the
	// choice labels render as plain_text buttons, which parse nothing.
	question = escapeMrkdwn(question)
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
	const text = "Sign in to Giant Swarm so I can act as you. " +
		"Until you do, I can't run tools on your behalf."
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
	return c.send(ctx, method, "application/json; charset=utf-8", string(data))
}

type slackResponse struct {
	OK    bool   `json:"ok"`
	Ts    string `json:"ts"`
	Error string `json:"error,omitempty"`
}

func (c *slackAPIClient) post(ctx context.Context, method string, params url.Values) (string, error) {
	return c.send(ctx, method, "application/x-www-form-urlencoded", params.Encode())
}

// rateLimitRetryCap bounds how long a Retry-After pause may hold a call;
// a longer server-requested wait fails the call instead of stalling the writer.
const rateLimitRetryCap = 30 * time.Second

// send executes one Slack Web API call and returns the ts of the affected
// message, for methods whose response carries one.
func (c *slackAPIClient) send(ctx context.Context, method, contentType, payload string) (string, error) {
	body, err := c.call(ctx, method, contentType, payload)
	if err != nil {
		return "", err
	}
	var result slackResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack %s: decode response: %w", method, err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack %s: %s", method, result.Error)
	}
	return result.Ts, nil
}

// call executes one Slack Web API POST and returns the raw response body. A
// 429 is retried once after honoring Retry-After, so a brief throttle does not
// abort the turn; a Retry-After longer than rateLimitRetryCap fails the call
// immediately rather than waiting it out.
func (c *slackAPIClient) call(ctx context.Context, method, contentType, payload string) ([]byte, error) {
	const maxAttempts = 2
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, strings.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("slack %s: build request: %w", method, err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+c.botToken)

		resp, err := http.DefaultClient.Do(req) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("slack %s: %w", method, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()
			wait := retryAfter(resp.Header)
			if attempt >= maxAttempts || wait > rateLimitRetryCap {
				return nil, fmt.Errorf("slack %s: rate limited (retry after %s)", method, wait)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("slack %s: read response: %w", method, err)
		}
		return body, nil
	}
}

// retryAfter reads the Retry-After header of a 429 response, defaulting to 1s
// when absent or unparsable.
func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || seconds < 0 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}
