package slack

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

const (
	batchInterval = 250 * time.Millisecond
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

	// adapter, slackUser, and connectorPrompts back the reactive connector
	// prompt: a core_auth_login tool result in the stream renders a Connect
	// button to slackUser. slackUser is the RAW Slack user ID ("U…"), never the
	// resolved email: chat.postEphemeral's user param requires a Slack ID, so an
	// email would fail with user_not_found. connectorPrompts is false when the UX
	// is disabled.
	adapter          *Adapter
	slackUser        string
	connectorPrompts bool
	// callToolInner maps a call_tool invocation's CallID to the inner muster
	// tool it targets, taken from the call arguments. Result deltas carry no
	// arguments, so this is how a call_tool result is attributed to
	// core_auth_login. Only touched from run()'s goroutine.
	callToolInner map[string]callToolTarget
	// loginURLs collects the backend login URLs surfaced as Connect buttons
	// this turn. flush scrubs them out of the agent's prose: the URL is a
	// single-use OAuth authorize link, and a second surface (or Slack's unfurl
	// crawler following it) can trip the auth server's reuse detection, which
	// revokes the user's whole token family. Only touched from run()'s
	// goroutine.
	loginURLs []string

	// turnUsage accumulates the per-LLM-call usage kagent reports across the
	// turn into the turn total. Only touched from run()'s goroutine.
	turnUsage channels.TurnUsage
	// toolsRendered counts tool-activity messages posted this turn so a tool-heavy
	// turn does not flood the thread (or hit Slack post rate limits). Only touched
	// from run()'s goroutine.
	toolsRendered int
	// toolPosts carries rendered tool-activity messages to a single poster
	// goroutine so a slow Slack API does not stall delta draining. Buffered to the
	// per-turn cap so enqueue never blocks; nil until the first tool post. Drained
	// before run() returns. toolWorkerDone closes when the poster exits.
	toolPosts      chan string
	toolWorkerDone chan struct{}

	mu            sync.Mutex
	buf           strings.Builder
	flushedLen    int                     // length of buf at the last chat.update; skips no-op flushes
	flushFailures int                     // consecutive failed ticker flushes; reset on success
	wroteAny      bool                    // set once the head message carries agent text; survives a partial multi-chunk flush
	promptDelta   *channels.OutboundDelta // set when stream ends on DeltaPrompt
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
	defer w.drainToolPosts() // flush any buffered tool-activity posts before returning

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case d, ok := <-ch:
			if !ok {
				return w.finalFlush(ctx)
			}
			if d.Usage != nil {
				// kagent reports usage per LLM call, so sum across the turn for
				// the turn total (the terminal event alone under-counts).
				w.turnUsage.InputTokens += d.Usage.InputTokens
				w.turnUsage.OutputTokens += d.Usage.OutputTokens
				w.turnUsage.TotalTokens += d.Usage.TotalTokens
			}
			if d.Err != nil {
				// Flush text buffered since the last tick before surfacing the
				// error, so wroteContent reflects all delivered content and the
				// failure note posts as a new message instead of overwriting it.
				if ferr := w.finalFlush(ctx); ferr != nil {
					w.logger.Warn("slack: flush before failure note failed", "error", ferr)
				}
				return d.Err
			}
			if d.Done {
				return w.finalFlush(ctx)
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
				w.maybeConnectorPrompt(d.Tool)
			case channels.DeltaPrompt:
				// Flush partial text so far, then hand off to the caller to post
				// the interactive approval prompt. A flush failure here is
				// non-fatal: the pending-task store and the prompt post do not
				// depend on the buffered prose, and failing the turn instead
				// would discard the paused task's only handle, leaving the A2A
				// task unresumable with a dangling tool call.
				if err := w.finalFlush(ctx); err != nil {
					w.logger.Warn("slack: flush at prompt handoff failed, buffered text lost", "error", err)
				}
				w.mu.Lock()
				w.promptDelta = &d
				w.mu.Unlock()
				return nil
			}

		case <-ticker.C:
			// A ticker flush is retryable: flushedLen only advances on success,
			// so the next tick re-sends the same content. Aborting on the first
			// error would discard the rest of a turn the agent completes anyway;
			// only a persistent failure gives up.
			if err := w.flush(ctx); err != nil {
				w.flushFailures++
				if w.flushFailures >= maxFlushFailures {
					return err
				}
				w.logger.Warn("slack: flush failed, retrying next tick", "failures", w.flushFailures, "error", err)
				continue
			}
			w.flushFailures = 0
		}
	}
}

// maxFlushFailures bounds consecutive ticker-flush failures before the turn is
// aborted. One transient Slack error must not kill a healthy stream; a Slack
// outage should not keep a doomed turn's thread slot busy either.
const maxFlushFailures = 3

// finalFlush retries a terminal flush (stream done, error, or prompt handoff)
// up to maxFlushFailures attempts. No later tick will re-send the buffered
// tail, so a single transient Slack error here would discard it even though
// the turn completed server-side; a short reply that never hits a ticker
// flush would otherwise be killable by one such error.
func (w *batchedWriter) finalFlush(ctx context.Context) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = w.flush(ctx); err == nil {
			return nil
		}
		if attempt >= maxFlushFailures || ctx.Err() != nil {
			return err
		}
		w.logger.Warn("slack: final flush failed, retrying", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(batchInterval):
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

// renderToolActivity queues a compact record of a tool call (and, at
// detailsFull, its result) when details are enabled. Rendered as a fenced code
// block so Slack collapses long payloads behind "show more". Capped per turn.
// The cap decision runs here (in run()'s goroutine); the HTTP post is handed to
// an async poster so a slow Slack API does not stall delta draining.
func (w *batchedWriter) renderToolActivity(ctx context.Context, tool *channels.ToolActivity) {
	if w.details == detailsOff || tool == nil {
		return
	}
	// The tool name is agent- and MCP-server-controlled text rendered inside a
	// code span; a backtick or newline in it would break out of the span and
	// inject markdown into the thread.
	name := codeSpanSafe(tool.Name)
	var md string
	switch tool.Kind {
	case channels.ToolCall:
		md = "🔧 `" + name + "`"
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
		md = "↳ `" + name + "` result\n```\n" + preview + "\n```"
	default:
		return
	}

	w.toolsRendered++
	switch {
	case w.toolsRendered > maxToolMessages+1:
		return // already queued the truncation note
	case w.toolsRendered == maxToolMessages+1:
		md = "_…tool-activity limit reached; hiding this turn's remaining tool posts. Details are still on: `/details off` mutes them entirely._"
	}

	w.enqueueToolPost(ctx, md)
}

var (
	authChallengeURLRe    = regexp.MustCompile(`https?://\S+`)
	authChallengeServerRe = regexp.MustCompile(`(?m)^\s*Server:\s*(\S+)`)
)

// maybeConnectorPrompt renders a Connect button when a core_auth_login tool
// result carries a backend login link. The link reaches the gateway only as
// free text in the agent's stream, so the URL is parsed out of it. Throttled
// per (user, backend) by the prompt cooldown; the post runs async on the
// adapter lifecycle context so a slow Slack API does not stall delta draining.
func (w *batchedWriter) maybeConnectorPrompt(tool *channels.ToolActivity) {
	if !w.connectorPrompts || tool == nil {
		return
	}
	if tool.Kind == channels.ToolCall {
		w.noteCallToolTarget(tool)
		return
	}
	if tool.Kind != channels.ToolResult || w.effectiveToolName(tool) != musterAuthLoginTool {
		return
	}
	server, loginURL := parseAuthChallengePayload(tool.Response, 0)
	if loginURL == "" {
		w.logger.Debug("slack: connector prompt skipped, no https login URL in auth challenge", "user", w.slackUser, "tool", tool.Name)
		return
	}
	w.loginURLs = append(w.loginURLs, loginURL)
	if server == "" {
		// The challenge text carries no "Server:" line; the call arguments
		// recorded for this CallID name the backend exactly.
		server = w.callToolInner[tool.CallID].server
	}
	if server == "" {
		server = "the requested tools"
	}
	if !w.adapter.markConnectorPrompted(w.slackUser, server, loginURL) {
		w.logger.Debug("slack: connector prompt skipped, cooldown active and URL unchanged", "user", w.slackUser, "server", server)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(w.adapter.baseCtx, connectorCheckTimeout)
		defer cancel()
		if err := w.client.postConnectorPrompt(ctx, w.channel, w.threadTS, w.slackUser, server, loginURL); err != nil {
			w.adapter.clearConnectorPrompted(w.slackUser, server)
			w.logger.Warn("slack: post connector prompt failed", "user", w.slackUser, "server", server, "error", err)
		}
	}()
}

// callToolTarget is the inner muster tool a call_tool invocation addresses:
// the tool name, and the backend server for tools that take one (such as
// core_auth_login, whose result text does not always name the server).
type callToolTarget struct {
	name   string
	server string
}

// noteCallToolTarget records the inner muster tool a call_tool invocation
// targets, keyed by CallID, so the matching result can be attributed to it.
func (w *batchedWriter) noteCallToolTarget(tool *channels.ToolActivity) {
	if tool.Name != musterCallToolMetaTool || tool.CallID == "" {
		return
	}
	inner, _ := tool.Args["name"].(string)
	if inner == "" {
		return
	}
	target := callToolTarget{name: inner}
	if arguments, ok := tool.Args["arguments"].(map[string]any); ok {
		target.server, _ = arguments["server"].(string)
	}
	if w.callToolInner == nil {
		w.callToolInner = make(map[string]callToolTarget)
	}
	w.callToolInner[tool.CallID] = target
}

// effectiveToolName resolves the muster tool a result belongs to: the stream's
// tool name directly, or the recorded inner target when the agent went through
// the call_tool meta-tool.
func (w *batchedWriter) effectiveToolName(tool *channels.ToolActivity) string {
	if tool.Name == musterCallToolMetaTool {
		if target, ok := w.callToolInner[tool.CallID]; ok {
			return target.name
		}
	}
	return tool.Name
}

// maxChallengePayloadDepth bounds the walk over a tool result payload; real
// payloads nest the challenge text at most a few levels down (direct
// {"output": text}, or an MCP content list under call_tool).
const maxChallengePayloadDepth = 6

// parseAuthChallengePayload walks a tool result payload's string values and
// returns the first auth challenge that carries a login URL. The challenge is
// free text whose nesting differs by call path, so every nested string is a
// candidate rather than assuming one key. Yields "" when no string carries a
// URL.
func parseAuthChallengePayload(v any, depth int) (server, loginURL string) {
	if depth > maxChallengePayloadDepth {
		return "", ""
	}
	switch t := v.(type) {
	case string:
		if s, u := parseAuthChallenge(t); u != "" {
			return s, u
		}
	case map[string]any:
		for _, e := range t {
			if s, u := parseAuthChallengePayload(e, depth+1); u != "" {
				return s, u
			}
		}
	case []any:
		for _, e := range t {
			if s, u := parseAuthChallengePayload(e, depth+1); u != "" {
				return s, u
			}
		}
	}
	return "", ""
}

// parseAuthChallenge extracts the backend name and login URL from a
// core_auth_login result. The URL is the first http(s) link, with trailing
// punctuation trimmed; the server comes from a "Server: <name>" line and is
// empty when the challenge does not name one (the caller falls back to the
// recorded call arguments). A missing or non-https URL yields "".
func parseAuthChallenge(output string) (server, loginURL string) {
	if m := authChallengeURLRe.FindString(output); m != "" {
		// Challenge text that embeds a JSON-encoded blob carries Go's HTML-safe
		// escaping, so each & arrives as the literal six characters \u0026; the
		// button must open the real URL.
		m = strings.ReplaceAll(m, jsonEscapedAmp, "&")
		loginURL = validLoginURL(strings.TrimRight(m, ").,]}>\"'"))
	}
	if m := authChallengeServerRe.FindStringSubmatch(output); m != nil {
		server = m[1]
	}
	return server, loginURL
}

// loginURLNote replaces a scrubbed login URL in the agent's prose.
const loginURLNote = "_(login link removed; use the Connect button above)_"

// scrubLoginURLs removes every login URL already surfaced as a Connect button
// from the agent's prose. The URL is a single-use OAuth authorize link:
// duplicating it in text lets a second click or Slack's unfurl crawler redeem
// or replay it, which the auth server answers by revoking the user's whole
// token family. Matching is by authorize-endpoint prefix (everything up to and
// including "?"): the agent re-encodes the query string freely (JSON-escaped
// ampersands, percent-encoded padding), so an exact match cannot be relied on.
// Markdown links carrying the URL are dropped wholesale so no dangling
// "[label]()" survives; bare occurrences (including Slack's "<url|label>"
// form) are replaced with a pointer at the button.
func (w *batchedWriter) scrubLoginURLs(text string) string {
	for _, loginURL := range w.loginURLs {
		prefix := loginURL
		if i := strings.IndexByte(prefix, '?'); i >= 0 {
			prefix = prefix[:i+1]
		}
		if !strings.Contains(text, prefix) {
			continue
		}
		quoted := regexp.QuoteMeta(prefix)
		text = regexp.MustCompile(`\[[^\]]*\]\(`+quoted+`[^)]*\)`).ReplaceAllString(text, loginURLNote)
		text = regexp.MustCompile(`<`+quoted+`[^>]*>`).ReplaceAllString(text, loginURLNote)
		text = regexp.MustCompile(quoted+`\S*`).ReplaceAllString(text, loginURLNote)
	}
	return text
}

// jsonEscapedAmp is how Go's HTML-safe JSON encoding spells "&" inside a
// string value.
const jsonEscapedAmp = `\u0026`

// validLoginURL returns raw when it is a well-formed absolute https URL with a
// host, and "" otherwise. The Connect button opens agent- and tool-controlled
// text as a browser URL, so anything that is not plainly https (http, a bare
// scheme, a malformed link) is rejected rather than rendered as a button.
func validLoginURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return raw
}

// enqueueToolPost buffers a rendered tool-activity message for the poster,
// starting it on first use. Called only from run()'s goroutine, so the lazy
// init is race-free. The buffer is sized to the per-turn cap, so the send never
// blocks the delta loop.
func (w *batchedWriter) enqueueToolPost(ctx context.Context, md string) {
	if w.toolPosts == nil {
		w.toolPosts = make(chan string, maxToolMessages+1)
		w.toolWorkerDone = make(chan struct{})
		go w.toolPoster(ctx)
	}
	w.toolPosts <- md
}

// toolPoster posts queued tool-activity messages in order. Best-effort: a post
// failure (including a cancelled ctx) is logged and never aborts the turn.
func (w *batchedWriter) toolPoster(ctx context.Context) {
	defer close(w.toolWorkerDone)
	for md := range w.toolPosts {
		if _, err := w.client.postMarkdown(ctx, w.channel, md, w.threadTS); err != nil {
			w.logger.Warn("slack: post tool activity failed", "error", err)
		}
	}
}

// drainToolPosts closes the queue and waits for the poster to finish, so all
// tool activity is flushed before the turn is considered done. No-op when no
// tool activity was queued. Idempotent: it clears the queue so a subsequent
// run() over the same writer (an auto-approved resume segment) starts a fresh
// poster instead of re-closing a closed channel.
func (w *batchedWriter) drainToolPosts() {
	if w.toolPosts == nil {
		return
	}
	close(w.toolPosts)
	<-w.toolWorkerDone
	w.toolPosts = nil
	w.toolWorkerDone = nil
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

// wroteContent reports whether any agent text has reached Slack. Used after the
// run loop to decide whether the failure note may overwrite the placeholder.
// flushedLen only advances once every chunk of a flush lands, so a multi-chunk
// flush whose head updated the placeholder but whose tail failed leaves it 0;
// wroteAny captures the head landing so that partial delivery still counts.
func (w *batchedWriter) wroteContent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushedLen > 0 || w.wroteAny
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
	text = w.scrubLoginURLs(text)

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
	// The head now carries agent text; mark it before the tail so a tail failure
	// (which leaves flushedLen at 0) does not let the failure note overwrite the
	// delivered head.
	w.mu.Lock()
	w.wroteAny = true
	w.mu.Unlock()
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
