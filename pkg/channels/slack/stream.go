package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

const (
	batchInterval = 250 * time.Millisecond
	slackAPIBase  = "https://slack.com/api"
	// downloadSizeMargin is the headroom over Slack's declared file size that a
	// download body may reach before it is rejected as an out-of-memory guard.
	downloadSizeMargin = 1 << 20
	// unknownSizeDownloadLimit caps a download whose declared size is unknown (0).
	// Without a baseline the size-plus-margin bound collapses to the margin alone
	// and would reject a legitimate larger file; this fixed ceiling is a pure
	// out-of-memory guard for that case, not a product limit.
	unknownSizeDownloadLimit = 16 << 20

	// methodChatPostMessage is the Web API method for new posts; it is special
	// in two spots (display identity, forced unfurl-off).
	methodChatPostMessage = "chat.postMessage"
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
	var md string
	switch tool.Kind {
	case channels.ToolCall:
		displayName, viaMuster := tool.Name, false
		args := tool.Args
		if inner, innerArgs, ok := unwrapCallTool(tool); ok {
			// Record the call→inner mapping so a detailsFull result (which
			// carries no Args) can resolve the inner name via effectiveToolName,
			// independent of whether connector prompts are enabled.
			w.noteCallToolTarget(tool)
			displayName, viaMuster, args = inner, true, innerArgs
		}
		md = "🔧 " + toolLabel(displayName)
		if viaMuster {
			md += " (via muster)"
		}
		if summary := compactJSON(args, toolArgsMax); summary != "" {
			md += "\n```\n" + summary + "\n```"
		}
	case channels.ToolResult:
		if w.details != detailsFull {
			return
		}
		resultName := w.effectiveToolName(tool)
		md = "↳ " + toolLabel(resultName) + " result"
		if tool.Name == musterCallToolMetaTool && resultName != tool.Name {
			md += " (via muster)"
		}
		preview := compactJSON(tool.Response, toolResultMax)
		if preview == "" {
			return
		}
		md += "\n```\n" + preview + "\n```"
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
	w.adapter.background(func(bg context.Context) {
		ctx, cancel := context.WithTimeout(bg, connectorCheckTimeout)
		defer cancel()
		if err := w.client.postConnectorPrompt(ctx, w.channel, w.threadTS, w.slackUser, server, loginURL); err != nil {
			w.adapter.clearConnectorPrompted(w.slackUser, server)
			w.logger.Warn("slack: post connector prompt failed", "user", w.slackUser, "server", server, "error", err)
		}
	})
}

// callToolTarget is the inner muster tool a call_tool invocation addresses:
// the tool name, and the backend server for tools that take one (such as
// core_auth_login, whose result text does not always name the server).
type callToolTarget struct {
	name   string
	server string
}

// toolLabel renders a tool name as a bold code span. The name is agent- and
// MCP-server-controlled text: a backtick or newline would break out of the code
// span and inject markdown into the thread, so it is sanitised here.
func toolLabel(name string) string {
	return "*`" + codeSpanSafe(name) + "`*"
}

// unwrapCallTool returns the inner muster tool name and arguments a call_tool
// invocation targets. ok is false unless the tool is call_tool and both the
// inner name and an arguments map are present, so callers fall back to the raw
// wrapped call.
func unwrapCallTool(tool *channels.ToolActivity) (name string, args map[string]any, ok bool) {
	if tool.Name != musterCallToolMetaTool {
		return "", nil, false
	}
	name, _ = tool.Args["name"].(string)
	args, hasArgs := tool.Args["arguments"].(map[string]any)
	if name == "" || !hasArgs {
		return "", nil, false
	}
	return name, args, true
}

// noteCallToolTarget records the inner muster tool a call_tool invocation
// targets, keyed by CallID, so the matching result can be attributed to it.
func (w *batchedWriter) noteCallToolTarget(tool *channels.ToolActivity) {
	if tool.CallID == "" {
		return
	}
	inner, args, ok := unwrapCallTool(tool)
	if !ok {
		return
	}
	target := callToolTarget{name: inner}
	target.server, _ = args["server"].(string)
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

// compactJSON marshals a tool payload to a single readable line: one space after
// each structural ':' and ',' (outside string literals), truncated to max runes.
// Returns "" for an empty or unmarshalable payload.
func compactJSON(v map[string]any, max int) string {
	if len(v) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	rs := []rune(spaceStructuralJSON(b))
	if len(rs) > max {
		return string(rs[:max]) + "…"
	}
	return string(rs)
}

// spaceStructuralJSON inserts one space after ':' and ',' that fall outside
// string literals in compact JSON, yielding a readable single line without
// indentation. String contents (which may themselves contain ':', ',' or
// escaped quotes) are left untouched.
func spaceStructuralJSON(b []byte) string {
	var out strings.Builder
	out.Grow(len(b) + len(b)/8)
	inString, escaped := false, false
	for _, c := range b {
		out.WriteByte(c)
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ':', ',':
			out.WriteByte(' ')
		}
	}
	return out.String()
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

// slackHTTPClient bounds every Slack Web API call. Without a timeout a
// blackholed connection blocks the calling goroutine indefinitely; some call
// sites hold the per-thread slot while calling (e.g. the users.info lookup
// during dispatch), so an unbounded hang would wedge the thread until process
// restart.
var slackHTTPClient = &http.Client{Timeout: 30 * time.Second}

// slackAPIClient is a minimal HTTP client for the Slack Web API.
type slackAPIClient struct {
	botToken string
	baseURL  string
	// username / iconURL, when set, post under a custom display identity
	// (chat:write.customize). Applied only to chat.postMessage and
	// chat.postEphemeral (chat.update keeps the original message's identity).
	username string
	iconURL  string
	// logger, when set, records download diagnostics at debug level. Nil in
	// tests and in call sites that never download.
	logger *slog.Logger
}

// applyIdentity adds the client's display identity (username/icon_url) via set.
// It is a no-op unless an identity is configured and the method is one that
// honours chat:write.customize (a new post, not an edit). Each field is applied
// only when non-empty: a name without an icon posts under the custom name and
// the Slack app's own icon (Slack keeps the app icon when icon_url is omitted),
// which is what we want while the AgentCard exposes a name but no icon.
func (c *slackAPIClient) applyIdentity(method string, set func(k, v string)) {
	if c.username == "" && c.iconURL == "" {
		return
	}
	if method != methodChatPostMessage && method != "chat.postEphemeral" {
		return
	}
	if c.username != "" {
		set(paramUsername, c.username)
	}
	if c.iconURL != "" {
		set(paramIconURL, c.iconURL)
	}
}

func (c *slackAPIClient) postMessage(ctx context.Context, channel, text, threadTS string) (string, error) {
	params := url.Values{
		paramChannel: {channel},
		paramText:    {text},
	}
	if threadTS != "" {
		params.Set(paramThreadTS, threadTS)
	}
	return c.post(ctx, methodChatPostMessage, params)
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

// authTest returns the bot's own Slack user ID via auth.test, used to
// recognise the bot's own channel-join event.
func (c *slackAPIClient) authTest(ctx context.Context) (string, error) {
	body, err := c.call(ctx, "auth.test", "application/x-www-form-urlencoded", "")
	if err != nil {
		return "", err
	}

	var result struct {
		OK     bool   `json:"ok"`
		Err    string `json:"error,omitempty"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack auth.test: decode: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack auth.test: %s", result.Err)
	}
	return result.UserID, nil
}

// threadInitiatorScanLimit bounds the conversations.replies page scanned for the
// first human author. A thread that opens with more leading bot messages than
// this falls back to first-poster seeding.
const threadInitiatorScanLimit = 50

// threadInitiator returns the user ID of the earliest human (non-bot) author in
// a thread, via conversations.replies (messages are returned oldest-first). A
// bot-authored root is skipped: bot messages carry bot_id (and often a user
// field naming the bot's own user), so they are not a human initiator; the
// first message without bot_id is the human who effectively started the thread.
// Returns "" when the thread is empty or its scanned prefix is all bot messages.
func (c *slackAPIClient) threadInitiator(ctx context.Context, channel, threadTS string) (string, error) {
	params := url.Values{
		paramChannel: {channel},
		paramTS:      {threadTS},
		"limit":      {strconv.Itoa(threadInitiatorScanLimit)},
	}
	body, err := c.call(ctx, "conversations.replies", "application/x-www-form-urlencoded", params.Encode())
	if err != nil {
		return "", err
	}

	var result struct {
		OK       bool   `json:"ok"`
		Err      string `json:"error,omitempty"`
		Messages []struct {
			User  string `json:"user"`
			BotID string `json:"bot_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack conversations.replies: decode: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack conversations.replies: %s", result.Err)
	}
	for _, m := range result.Messages {
		if m.BotID == "" && m.User != "" {
			return m.User, nil
		}
	}
	return "", nil
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
// top-level text is the notification/accessibility fallback; it is mrkdwn-parsed
// by Slack, so agent output must be escaped there even though the markdown block
// itself must not be.
func (c *slackAPIClient) postMarkdown(ctx context.Context, channel, md, threadTS string) (string, error) {
	body := map[string]any{
		paramChannel: channel,
		paramText:    escapeMrkdwn(md),
		paramBlocks:  markdownBlocks(md),
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// chatUpdateMarkdown replaces a message's content with a markdown block. The
// top-level fallback text is escaped for the same reason as in postMarkdown.
func (c *slackAPIClient) chatUpdateMarkdown(ctx context.Context, channel, ts, md string) error {
	body := map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramText:    escapeMrkdwn(md),
		paramBlocks:  markdownBlocks(md),
	}
	_, err := c.postJSON(ctx, "chat.update", body)
	return err
}

// postApprovalPrompt posts a Block Kit message with ✅/❌ buttons for HITL
// approval. The button values encode the thread (routing) and the task the
// prompt renders (staleness check).
func (c *slackAPIClient) postApprovalPrompt(ctx context.Context, channel, threadID, taskID, promptText string) error {
	text := "_Waiting for approval…_"
	if promptText != "" {
		// promptText is agent-rendered (tool name, args, hint) and enters an
		// mrkdwn section block.
		text = truncateRunes(escapeMrkdwn(promptText), slackSectionTextMax)
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
						bkValue:    encodeHitlValue(threadID, taskID),
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "❌ Deny"},
						bkStyle:    bkDanger,
						bkActionID: hitlDeny,
						bkValue:    encodeHitlValue(threadID, taskID),
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "💬 Chat"},
						bkActionID: hitlChat,
						bkValue:    encodeHitlValue(threadID, taskID),
					},
				},
			},
		},
	}
	_, err := c.postJSON(ctx, methodChatPostMessage, body)
	return err
}

// questionSection renders an ask_user question as a bold mrkdwn section block.
// The question is agent-authored; the two asterisks count against Slack's
// 3000-char section limit.
func questionSection(question string) map[string]any {
	question = truncateRunes(escapeMrkdwn(question), slackSectionTextMax-2)
	return map[string]any{
		bkType: bkSection,
		bkText: map[string]any{bkType: bkMrkdwn, bkText: "*" + question + "*"},
	}
}

// submitActions renders the Submit button that commits a widget selection. Its
// value encodes the thread (routing) and the task the prompt renders
// (staleness check).
func submitActions(threadID, taskID string) map[string]any {
	return map[string]any{
		bkType: bkActions,
		bkElements: []any{
			map[string]any{
				bkType:     bkButton,
				bkText:     map[string]any{bkType: bkPlainText, bkText: "Submit"},
				bkStyle:    bkPrimary,
				bkActionID: hitlSubmit,
				bkValue:    encodeHitlValue(threadID, taskID),
			},
		},
	}
}

// choiceOptions builds the Block Kit option objects for a question's choices,
// each valued by its choice index. Labels are capped at the option-text limit;
// the caller routes longer labels to the section layout, so truncation never
// bites in practice.
func choiceOptions(choices []string) []any {
	options := make([]any, 0, len(choices))
	for i, choice := range choices {
		options = append(options, map[string]any{
			bkText:  map[string]any{bkType: bkPlainText, bkText: truncateRunes(choice, choiceLabelWidgetMax)},
			bkValue: strconv.Itoa(i),
		})
	}
	return options
}

// choiceWidgetBlock builds a radio_buttons (single-select) or checkboxes
// (multi-select) actions block for one question's choices. blockID lets the
// interaction handler locate the selection under state.values; every widget
// shares the hitlGroup action_id.
func choiceWidgetBlock(blockID string, choices []string, multiple bool) map[string]any {
	elementType := bkRadioButtons
	if multiple {
		elementType = bkCheckboxes
	}
	return map[string]any{
		bkType:    bkActions,
		bkBlockID: blockID,
		bkElements: []any{
			map[string]any{
				bkType:     elementType,
				bkActionID: hitlGroup,
				bkOptions:  choiceOptions(choices),
			},
		},
	}
}

// postChoiceWidgetPrompt posts an ask_user question as a vertical
// radio_buttons (single-select) or checkboxes (multi-select) widget plus a
// Submit button. Each option's value is its choice index; the interaction
// handler reads the selection out of state.values on Submit.
func (c *slackAPIClient) postChoiceWidgetPrompt(ctx context.Context, channel, threadID, taskID, question string, choices []string, multiple bool) error {
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     truncateRunes(escapeMrkdwn(question), slackSectionTextMax),
		paramBlocks: []any{
			questionSection(question),
			choiceWidgetBlock(hitlGroupBlock, choices, multiple),
			submitActions(threadID, taskID),
		},
	}
	_, err := c.postJSON(ctx, methodChatPostMessage, body)
	return err
}

// postChoiceFormPrompt posts a multi-question ask_user prompt as a single form:
// a question section plus a radio/checkbox widget per question, all committed by
// one Submit. Each question's widget block_id encodes its question index
// (hitlQGroupPrefix + "_<qi>") so the handler maps each selection back to its
// question. The caller (formRenderable) guarantees every question is widgetable.
func (c *slackAPIClient) postChoiceFormPrompt(ctx context.Context, channel, threadID, taskID string, questions []channels.HitlQuestion) error {
	blocks := make([]any, 0, 2*len(questions)+1)
	for qi, q := range questions {
		blocks = append(blocks, questionSection(q.Question))
		blocks = append(blocks, choiceWidgetBlock(fmt.Sprintf("%s_%d", hitlQGroupPrefix, qi), q.Choices, q.Multiple))
	}
	blocks = append(blocks, submitActions(threadID, taskID))
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     "Please answer the questions below.",
		paramBlocks:   blocks,
	}
	_, err := c.postJSON(ctx, "chat.postMessage", body)
	return err
}

// postChoiceSectionPrompt posts an ask_user question whose choices are too long
// for a widget option's 75-rune text: one section block per choice carries the
// full label (up to Slack's 3000-char section limit) with a selection control.
// Single-select uses an accessory button per row (a click commits, since one
// choice per row is unambiguous); multi-select uses an accessory single-option
// checkbox per row plus a Submit button, and the handler gathers the selected
// rows out of state.values.
func (c *slackAPIClient) postChoiceSectionPrompt(ctx context.Context, channel, threadID, taskID, question string, choices []string, multiple bool) error {
	blocks := []any{questionSection(question)}
	for i, choice := range choices {
		section := map[string]any{
			bkType: bkSection,
			bkText: map[string]any{bkType: bkMrkdwn, bkText: truncateRunes(escapeMrkdwn(choice), slackSectionTextMax)},
		}
		if multiple {
			section[bkBlockID] = fmt.Sprintf("%s_%d", hitlGroupBlock, i)
			section[bkAccessory] = map[string]any{
				bkType:     bkCheckboxes,
				bkActionID: hitlGroup,
				bkOptions: []any{
					map[string]any{
						bkText:  map[string]any{bkType: bkPlainText, bkText: "Select"},
						bkValue: strconv.Itoa(i),
					},
				},
			}
		} else {
			section[bkAccessory] = map[string]any{
				bkType:     bkButton,
				bkText:     map[string]any{bkType: bkPlainText, bkText: "Select"},
				bkActionID: fmt.Sprintf("%s_%d", hitlChoice, i),
				bkValue:    encodeChoiceValue(threadID, taskID, i),
			}
		}
		blocks = append(blocks, section)
	}
	if multiple {
		blocks = append(blocks, submitActions(threadID, taskID))
	}
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     truncateRunes(escapeMrkdwn(question), slackSectionTextMax),
		paramBlocks:   blocks,
	}
	_, err := c.postJSON(ctx, "chat.postMessage", body)
	return err
}

// postSignInPrompt posts a Block Kit message with a "Sign in" button linking
// to linkURL, and returns the posted message's ts. It is used to
// nudge an unlinked Slack user into the OBO account-linking flow. A real
// threaded message, never an ephemeral: for a root channel mention the prompt
// is the thread's first visible reply (a thread-scoped ephemeral there is never
// surfaced by Slack), in an assistant DM only thread replies render in the
// assistant pane, and the returned ts lets the prompt be rewritten in place
// once the link completes.
func (c *slackAPIClient) postSignInPrompt(ctx context.Context, channel, threadID, user, linkURL string) (string, error) {
	// The prompt is a public thread message and its link is minted for one
	// user: address it, or a bystander clicks a button bound to someone else's
	// identity and lands on the email-mismatch page.
	text := "Sign in so I can act as you. " +
		"Until you do, I can't run tools on your behalf."
	if user != "" {
		text = "<@" + user + "> " + text
	}
	body := map[string]any{
		paramChannel: channel,
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
						bkText:     map[string]any{bkType: bkPlainText, bkText: "Sign in"},
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
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// slackSectionTextMax is Slack's limit on a section block's text object; a
// longer text gets the whole message rejected with invalid_blocks.
const slackSectionTextMax = 3000

// postConnectorPrompt posts an ephemeral (target-user-only) Block Kit message
// offering to connect a muster backend the agent cannot use for the user yet:
// a "Connect <server>" URL button opening loginURL plus a "Not now" dismissal.
// When threadID is set the prompt is posted in-thread.
func (c *slackAPIClient) postConnectorPrompt(ctx context.Context, channel, threadID, user, server, loginURL string) error {
	text := fmt.Sprintf("The agent can't use *%s* for you yet. Connect your account once so those tools work.", escapeMrkdwn(server))
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
						bkText:     map[string]any{bkType: bkPlainText, bkText: truncateButtonLabel("Connect " + server)},
						bkStyle:    bkPrimary,
						bkActionID: connectorConnect,
						bkValue:    server,
						bkURL:      loginURL,
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "Not now"},
						bkActionID: connectorDismiss,
						bkValue:    server,
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

// postEphemeralText posts a plain in-thread message visible only to user.
func (c *slackAPIClient) postEphemeralText(ctx context.Context, channel, user, threadTS, text string) error {
	body := map[string]any{
		paramChannel: channel,
		paramUser:    user,
		paramText:    text,
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	_, err := c.postJSON(ctx, "chat.postEphemeral", body)
	return err
}

// postAccessConsentPrompt posts the ephemeral (initiator-only) "is <newcomer>
// allowed?" prompt with Yes/No buttons. Only the initiator receives it, so only
// the initiator can click. The button value encodes the thread and the newcomer
// so the interaction handler resolves the right parked request.
func (c *slackAPIClient) postAccessConsentPrompt(ctx context.Context, channel, threadID, initiator, newcomer string) error {
	// A thread is one shared session that, on kagent v0.9.9, acts under the
	// initiator's identity even after others are allowed in (per-user identity
	// is the kagent-dev/kagent#1933 + #2181 fix). So the grant does let the
	// newcomer drive the agent on the initiator's behalf; the wording says so.
	text := fmt.Sprintf("Is <@%s> allowed to instruct the agent to work on your behalf in this thread?", newcomer)
	value := encodeAccessValue(threadID, newcomer)
	body := map[string]any{
		paramChannel:  channel,
		paramUser:     initiator,
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
						bkText:     map[string]any{bkType: bkPlainText, bkText: "✅ Yes"},
						bkStyle:    bkPrimary,
						bkActionID: accessAllow,
						bkValue:    value,
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "❌ No"},
						bkStyle:    bkDanger,
						bkActionID: accessDeny,
						bkValue:    value,
					},
				},
			},
		},
	}
	_, err := c.postJSON(ctx, "chat.postEphemeral", body)
	return err
}

// interactionHTTPClient bounds POSTs to a Slack interaction response_url. These
// run on the adapter's long-lived context (routeInteraction), so without a
// timeout a hung upstream would park the goroutine until process shutdown.
var interactionHTTPClient = &http.Client{Timeout: 10 * time.Second}

// respondURL replaces a message via a Slack interaction response_url. Ephemeral
// messages have no addressable ts for chat.update, so the access-consent prompt
// is updated this way after a click. The response_url is unauthenticated and
// short-lived; a failure is non-fatal (the decision has already been recorded).
func respondURL(ctx context.Context, responseURL, threadTS, text string) error {
	if responseURL == "" {
		return nil
	}
	payload := map[string]any{
		"replace_original": true,
		"response_type":    "ephemeral",
		paramText:          text,
	}
	// A response_url replacement of a thread-scoped ephemeral must carry the
	// thread_ts of the source, or Slack renders the replacement at channel
	// top level as well as in the thread.
	if threadTS != "" {
		payload[paramThreadTS] = threadTS
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack respond_url: marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("slack respond_url: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := interactionHTTPClient.Do(req) //nolint:gosec
	if err != nil {
		return fmt.Errorf("slack respond_url: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("slack respond_url: http status %d", resp.StatusCode)
	}
	return nil
}

// truncateButtonLabel keeps a button label within Slack's 75-character limit.
func truncateButtonLabel(s string) string {
	return truncateRunes(s, 75)
}

// truncateRunes caps s at max runes, replacing the tail with an ellipsis.
// Counting runes (not bytes) means a multi-byte glyph is never split mid-rune.
func truncateRunes(s string, max int) string {
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
	// The identity fields go onto a clone so the caller's map stays untouched.
	if m, ok := body.(map[string]any); ok {
		cloned := maps.Clone(m)
		c.applyIdentity(method, func(k, v string) {
			if _, exists := cloned[k]; !exists {
				cloned[k] = v
			}
		})
		if method == methodChatPostMessage {
			// Bot posts relay agent- and tool-controlled links; an unfurl has
			// Slack's crawler fetch them, which for single-use auth links can
			// trip the auth server's replay detection.
			cloned[paramUnfurlLinks] = false
			cloned[paramUnfurlMedia] = false
		}
		body = cloned
	}
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
	c.applyIdentity(method, params.Set)
	if method == methodChatPostMessage {
		params.Set(paramUnfurlLinks, "false")
		params.Set(paramUnfurlMedia, "false")
	}
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
// 429 is retried honoring Retry-After: rate limiting is a pacing signal, not a
// turn-fatal error, and a multi-chunk flush plus tool posts can draw several
// consecutive 429s against chat.postMessage's ~1 msg/sec/channel limit. A
// Retry-After longer than rateLimitRetryCap, or the attempt budget running
// out, fails the call rather than waiting it out. Any other non-2xx status is
// an error carrying the status code, not a JSON decode attempt on a non-API
// body.
func (c *slackAPIClient) call(ctx context.Context, method, contentType, payload string) ([]byte, error) {
	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, strings.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("slack %s: build request: %w", method, err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+c.botToken)

		resp, err := slackHTTPClient.Do(req) //nolint:gosec
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
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("slack %s: http status %d", method, resp.StatusCode)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("slack %s: read response: %w", method, err)
		}
		return body, nil
	}
}

// downloadFile fetches a Slack file's bytes from an authenticated url_private.
// sizeHint is Slack's declared file size; the body read is bounded to it plus a
// small margin purely as an out-of-memory guard against a mismatched or hostile
// response, not as a product limit on attachment size.
func (c *slackAPIClient) downloadFile(ctx context.Context, fileURL, declaredType string, sizeHint int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("slack download: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)
	// Signal a raw-file (not browser) fetch. A request Slack reads as a browser
	// navigation is bounced to the web sign-in page instead of the bytes.
	req.Header.Set("Accept", "*/*")

	resp, err := slackHTTPClient.Do(req) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("slack download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("slack download: http status %d", resp.StatusCode)
	}

	limit := int64(sizeHint) + downloadSizeMargin
	if sizeHint <= 0 {
		limit = unknownSizeDownloadLimit
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("slack download: read body: %w", err)
	}
	if int64(len(body)) >= limit {
		return nil, fmt.Errorf("slack download: body exceeds %d bytes", limit)
	}

	// An unauthorized url_private is answered with the Slack web sign-in page, not
	// an error status, and the Content-Type varies (text/html on a bare redirect,
	// but application/force-download or text/plain when the download path is hit
	// with a rejected token). Detect it by body so every variant is caught, and
	// fail rather than base64-forwarding the login page to the agent.
	if looksLikeSlackSignIn(body) {
		c.logDownload("slack: attachment download returned sign-in page, not file bytes", fileURL, resp, declaredType, len(body))
		return nil, fmt.Errorf("slack download: got the Slack sign-in page instead of file bytes (download reached files.slack.com unauthenticated)")
	}

	c.logDownload("slack: attachment download ok", fileURL, resp, declaredType, len(body))
	return body, nil
}

// logDownload records download diagnostics at debug level: the response status
// and Content-Type, the declared file type, whether the request was redirected
// (final URL differs from the requested one — the stdlib drops Authorization on
// a cross-host redirect, a common cause of an unauthenticated landing), and
// whether a bearer token was attached. The token itself is never logged.
func (c *slackAPIClient) logDownload(msg, fileURL string, resp *http.Response, declaredType string, bodyLen int) {
	if c.logger == nil {
		return
	}
	c.logger.Debug(msg,
		"status", resp.StatusCode,
		"response_type", resp.Header.Get("Content-Type"),
		"declared_type", declaredType,
		"redirected", resp.Request.URL.String() != fileURL,
		"final_host", resp.Request.URL.Hostname(),
		"auth_attached", c.botToken != "",
		"body_len", bodyLen)
}

// looksLikeSlackSignIn reports whether body is Slack's web sign-in / redirect
// page rather than real file bytes. Slack serves this page (HTTP 200) for an
// unauthorized url_private download; its markers are stable across the
// Content-Type variants Slack uses for it.
func looksLikeSlackSignIn(body []byte) bool {
	const sniff = 1024
	head := body
	if len(head) > sniff {
		head = head[:sniff]
	}
	lower := strings.ToLower(string(head))
	if !strings.HasPrefix(strings.TrimSpace(lower), "<!doctype html") && !strings.HasPrefix(strings.TrimSpace(lower), "<html") {
		return false
	}
	return strings.Contains(lower, "slack-edge.com") || strings.Contains(lower, "data-primer") || strings.Contains(lower, "signin")
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
