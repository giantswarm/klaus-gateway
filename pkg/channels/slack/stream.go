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
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

const (
	// streamAppendInterval paces chat.appendStream while a turn's answer
	// streams. The first text of a turn goes out at once so the message appears
	// while the agent is still writing; everything after it rides this tick, so
	// one active thread costs about 60 appends a minute — comfortably inside the
	// method's tier-4 budget, with the 429 handling in call() as the safety net.
	streamAppendInterval = time.Second
	// finalFlushRetryDelay spaces the attempts of the terminal flush.
	finalFlushRetryDelay = 250 * time.Millisecond
	// streamCloseTimeout bounds closing a stream a cancelled turn left open.
	streamCloseTimeout = 5 * time.Second
	slackAPIBase       = "https://slack.com/api"
	// downloadSizeMargin is the headroom over Slack's declared file size that a
	// download body may reach before it is rejected as an out-of-memory guard.
	downloadSizeMargin = 1 << 20
	// unknownSizeDownloadLimit caps a download whose declared size is unknown (0).
	// Without a baseline the size-plus-margin bound collapses to the margin alone
	// and would reject a legitimate larger file; this fixed ceiling is a pure
	// out-of-memory guard for that case, not a product limit.
	unknownSizeDownloadLimit = 16 << 20
	// maxAttachmentDownload is a hard per-file ceiling on attachment bytes. The
	// declared-size-plus-margin bound alone is defeated by an honestly declared
	// huge file (Slack allows uploads up to 1 GB), which would be fully buffered
	// — then base64-inflated in the A2A payload — only for the agent to reject
	// it. A file declared above this ceiling is refused before the GET is sent.
	maxAttachmentDownload = 32 << 20
	// attachmentDownloadConcurrency bounds parallel per-file downloads within
	// one message, so a multi-file message is not serialized behind one slow
	// fetch while the thread slot is held.
	attachmentDownloadConcurrency = 4
	// attachmentDownloadBudget bounds the total time one message's attachment
	// downloads may hold the thread slot; files still in flight when it expires
	// are dropped (with a notice), not retried.
	attachmentDownloadBudget = 2 * time.Minute

	// methodChatPostMessage is the Web API method for new posts; it is special
	// in two spots (display identity, forced unfurl-off).
	methodChatPostMessage = "chat.postMessage"
	// methodSetSessionStatus is the agent-messaging method that drives the
	// session lifecycle (and creates the session when it does not exist yet).
	methodSetSessionStatus = "agents.sessions.setStatus"
	// The streaming methods that render a turn's answer: one message opened,
	// appended to while the agent writes, and closed with the session's status.
	methodChatStartStream  = "chat.startStream"
	methodChatAppendStream = "chat.appendStream"
	methodChatStopStream   = "chat.stopStream"
	// slackMarkdownBlockMax caps the text of one Block Kit markdown block and
	// of one streamed message, Slack's 12 000-char limit. A narration post is
	// split under it by splitMarkdown, fence close and reopen included; a
	// streamed answer is cut at whitespace by cutPiece and rolls over into a
	// new message when it reaches the cap.
	slackMarkdownBlockMax = 12000
	// slackFallbackTextMax caps a message's top-level text: chat.update refuses
	// a text field over 4 000 characters (msg_too_long). On a markdown-block
	// message that field is only the notification and accessibility fallback of
	// the block carrying the reply, so the fallback is cut, never the reply.
	slackFallbackTextMax = 4000

	// Slack Web API error codes the adapter reacts to.
	errCodeMissingScope        = "missing_scope"
	errCodeInvalidArguments    = "invalid_arguments"
	errCodeNotAllowedTokenType = "not_allowed_token_type"
	errCodeFeatureDisabled     = "feature_disabled"
	// Streaming rejections: the user pressed Stop, the message is no longer
	// streaming, or the message belongs to another app.
	errCodeStoppedByUser       = "stopped_by_user"
	errCodeNotInStreamingState = "message_not_in_streaming_state"
	errCodeMsgNotOwned         = "message_not_owned_by_app"
)

// batchedWriter accumulates OutboundDelta content and streams it into one
// Slack message: chat.startStream opens the answer, chat.appendStream adds the
// text accumulated since the last tick, and chat.stopStream closes it with the
// agent session's exit status. Each append carries only the new text, Slack
// animates the message while the stream is open, and the Stop button ends the
// stream on Slack's side.
//
// The main reply accumulates DeltaText content. Tool activity is a live
// status ticker at the default details level — one muted line that updates in
// place while the agent works and collapses to a one-line receipt — and, at
// detailsFull, additionally an aggregated audit message of per-call
// context-block entries. The live line is a message on every surface: Slack's
// native working indicator (the agent session status, setSessionStatus) says
// only THAT the agent is working — the method carries no free text — so the
// ticker message stays as the detail carrier saying what it is working on.
// The ticker is per-segment: narration closes the live
// ticker into its receipt at its position (counting only that segment's
// steps), and the next tool call opens a fresh ticker message below the
// narration, so a long turn reads narration → receipt → narration → receipt in
// stream order. The agent's interim narration posts as its own in-thread
// messages, except that at the default level a short narration folds into the
// segment's status message as a muted context block above the ticker line, so
// a typical narrate-then-call group is one compact message. The main reply's
// stream opens on its first text and therefore lands last.
//
// When the stream ends on a DeltaPrompt, run() captures it in promptDelta
// (flushing the main buffer first) and returns nil. The caller is responsible
// for posting the approval prompt and registering the pending task.
type batchedWriter struct {
	client   *slackAPIClient
	channel  string
	threadTS string // thread root — used when posting the status message
	// placeholderTS is the text-mode progress placeholder ("thinking…"), empty
	// in reactions mode. A stream cannot take over an existing message the way
	// chat.update did, so the placeholder is deleted once the answer has a
	// message of its own.
	placeholderTS string
	logger        *slog.Logger
	details       detailsLevel // tool-activity verbosity snapshotted at turn start

	// adapter, slackUser, and connectorPrompts back the reactive connector
	// prompt: a core_auth_login tool result in the stream renders a Connect
	// button to slackUser. slackUser is the RAW Slack user ID ("U…"), never the
	// resolved email: chat.postEphemeral's user param requires a Slack ID, so an
	// email would fail with user_not_found. connectorPrompts is false when the UX
	// is disabled.
	adapter          *Adapter
	slackUser        string
	connectorPrompts bool
	// recipientTeam is the workspace of the person the answer is for (the bot's
	// own team, from auth.test). Slack requires it next to the recipient user on
	// a stream opened in a channel; both are left off in a DM. Slack Connect
	// channels, where the two teams differ, are out of scope.
	recipientTeam string
	// sessionTitle names the agent session in Slack's Messages tab. It is set
	// only on the turn that opens the conversation — Slack applies a title when
	// the status call creates the session and ignores it afterwards — so on
	// every later turn it is empty and no title is sent.
	sessionTitle string
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
	// connectorManualSignIn is set when a connector prompt this turn could not
	// wire the auto-resume callback (no public base URL, or decoration failed),
	// so the agent's sign-in narration must survive: the user has to sign in and
	// say so by hand. Only touched from run()'s goroutine.
	connectorManualSignIn bool

	// turnUsage accumulates the per-LLM-call usage kagent reports across the
	// turn into the turn total. Only touched from run()'s goroutine.
	turnUsage channels.TurnUsage
	// timer is the turn's timeline (from run's context; nil-safe): the first
	// text delta, the characters streamed and the tool calls are recorded on
	// it as the deltas arrive.
	timer *channels.TurnTimer
	// toolsRendered counts detailsFull tool-activity entries this turn so a
	// tool-heavy turn does not flood the thread (or hit Slack post rate limits).
	// Only touched from run()'s goroutine.
	toolsRendered int
	// narrationsRendered counts narration renderings this turn (own messages
	// and blocks folded into the status message), capped like tool activity so
	// a long tool-calling loop does not flood the thread. Only touched from
	// run()'s goroutine.
	narrationsRendered int
	// toolLogTurn is this turn's ordinal in the adapter's per-thread tool log
	// (see inspect.go), opened lazily on the first recorded entry; 0 until
	// then. Only touched from run()'s goroutine.
	toolLogTurn int
	// threadPosts carries rendered in-thread messages (tool activity and
	// narration) to a single poster goroutine so a slow Slack API does not stall
	// delta draining. Buffered to the per-turn caps so enqueue never blocks; nil
	// until the first post. Drained before the main reply lands.
	// threadPosterDone closes when the poster exits.
	threadPosts      chan threadPost
	threadPosterDone chan struct{}

	mu            sync.Mutex
	pending       string    // agent text accepted but not yet appended to the stream
	flushFailures int       // consecutive failed ticker flushes; reset on success
	flushRetryAt  time.Time // ticker flushes wait until here once they keep failing
	// appendedLen counts the answer bytes this writer has handed to streams,
	// summed across roll-overs. It is what a process continuing the turn after
	// a restart must not post again, and what says the reply carries agent text.
	appendedLen int
	promptDelta *channels.OutboundDelta // set when stream ends on DeltaPrompt
	// Stream state, touched from run()'s goroutine (and from the terminal flush
	// the adapter runs once run() has returned). streamTS is the open streamed
	// message, "" when none is open; streamed counts the characters it carries,
	// so the answer rolls over into a fresh message before Slack's per-message
	// cap; streamMessages lists every streamed message of the turn, which is
	// what a retract deletes.
	streamTS       string
	streamed       int
	streamMessages []string
	// streamAdopted marks streamTS as a stream a previous process opened
	// (continueFrom), which this one has not written to yet: its first text
	// goes out as an append, so a stream Slack has closed since the restart
	// recovers onto a message of its own instead of failing the closing stop.
	streamAdopted bool
	// streamRecovered marks a stream Slack closed under us as already reopened
	// once this turn; streamStopped closes the text path quietly for the rest
	// of the turn (the user pressed Stop); streamFailed closes it as a failure
	// (Slack kept closing the stream), so the terminal flush reports the reply
	// as incomplete instead of ending the turn as if it were whole;
	// exitStatusSent marks the session's exit status as delivered by the stop
	// that closed the stream, so run()'s exit call does not repeat it.
	streamRecovered bool
	streamStopped   bool
	streamFailed    bool
	exitStatusSent  bool
	// narrationTS holds the timestamps of the narration posted after a login
	// challenge this turn, so a connector prompt taking over can retract that
	// sign-in prose with the reply. Written by the poster goroutine.
	narrationTS []string

	// Tool-status ticker state (the detailsOn rendering) for the CURRENT
	// segment: written under mu on run()'s goroutine as tool calls stream, read
	// by the poster to render the live ticker. Kept outside the poster so a
	// dropped ticker kick never loses a step: the next render always sees exact
	// counts. Narration closes a segment by snapshotting and resetting these
	// atomically (takeToolStatus) on run()'s goroutine, so steps recorded after
	// the narration in the delta stream count toward the next segment.
	toolSteps   int
	toolCurrent string         // display name of the segment's most recent call
	toolOrder   []string       // distinct display names in segment first-use order
	toolCounts  map[string]int // segment calls per display name

	// Continuation across a restart (continuation.go). carried is what the
	// previous process delivered of the turn this writer continues; skipText
	// is how much answer text is still to be dropped, trimLead whether the
	// whitespace the cut left in front is still to go, and leadTrimmed (under
	// mu) how much of it went; carriedSteps is the step count the open
	// segment was seeded with, so a segment closed without a new step posts
	// no second receipt. onDelivered, when set, receives the delivery record
	// after every flush and every step.
	carried      store.Delivered
	skipText     int
	trimLead     bool
	leadTrimmed  int
	carriedSteps int
	onDelivered  func(ctx context.Context, d store.Delivered)
	// ran is set by run(): a second cycle over the same writer resets the
	// ticker counters the previous cycle's closing pass left standing.
	ran bool
}

// toolReceipt is one closed ticker segment's exact counters. It is snapshotted
// and reset atomically on run()'s goroutine when narration closes the segment,
// and travels to the poster on the narration's threadPost, so the receipt
// renders exact per-segment counts no matter how far the poster lags.
type toolReceipt struct {
	steps  int
	order  []string
	counts map[string]int
}

// threadPostKind selects how the poster lands one queued item: narration is
// its own full-weight message (or, when marked foldable and no ticker is live
// yet, a muted block inside the status message); a tool entry (detailsFull) is
// one context block appended to the running activity message; a status kick
// (detailsOn) refreshes the live ticker from the writer's counters. The zero
// value is narration so the retract bookkeeping (which only narration uses)
// matches the zero-value struct.
type threadPostKind int

const (
	postNarration threadPostKind = iota
	postToolEntry
	postToolStatusKick
)

// threadPost is one rendered in-thread item, posted outside the main reply.
// retract marks the narration a connector prompt taking over the turn deletes
// along with the reply. fold marks narration short enough to render as a muted
// context block inside the segment's status message instead of a full-weight
// message of its own. receipt, set on the first rendered chunk of a narration
// that follows tool calls, carries the ticker segment the narration closes:
// the poster collapses the live status message into this receipt at its
// position before rendering the narration below it.
type threadPost struct {
	kind    threadPostKind
	md      string
	retract bool
	fold    bool
	receipt *toolReceipt
}

func newBatchedWriterWithClient(client *slackAPIClient, channel, ts, threadTS string, details detailsLevel, logger *slog.Logger) *batchedWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &batchedWriter{
		client:        client,
		channel:       channel,
		placeholderTS: ts,
		threadTS:      threadTS,
		details:       details,
		logger:        logger,
	}
}

// run drains deltas from ch, appending the agent's text to the turn's streamed
// message at streamAppendInterval.
func (w *batchedWriter) run(ctx context.Context, ch <-chan channels.OutboundDelta) error {
	w.timer = channels.TurnTimerFromContext(ctx)
	// A run() cycle over a writer that ran before (an auto-approved prompt
	// resuming the turn in place) starts a fresh ticker: the previous cycle's
	// segment collapsed into its receipt when its poster closed. Its answer
	// stream was closed by that cycle's final stop, so the continuation opens a
	// message of its own rather than reopening a closed one.
	if w.ran {
		w.resetToolStatus()
		w.resetStream()
	}
	w.ran = true
	ticker := time.NewTicker(streamAppendInterval)
	defer ticker.Stop()
	// The session leaves "processing" on EVERY exit — stream done, stream error,
	// /stop, and the HITL prompt pause. Slack's agent loading UX does not clear
	// itself when the app posts any more, so a missing exit status leaves the
	// thread spinning for up to an hour. The stop that closes the reply's stream
	// carries that status already, so this call covers the turns that never
	// opened a stream and the stops that failed. Registered before the drain so
	// it lands after the turn's last in-thread post.
	w.setSessionStatus(ctx, sessionProcessing)
	defer func() {
		if w.exitStatusSent {
			return
		}
		w.setSessionStatus(ctx, w.exitSessionStatus())
	}()
	defer w.endStream(ctx)     // backstop for the ctx.Done() exit; finalFlush closes first
	defer w.drainThreadPosts() // backstop for the ctx.Done() exit; finalFlush drains first

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case d, ok := <-ch:
			if !ok {
				// The producer closes without a terminal delta when the turn
				// context ended under it; both cases are ready then, so the
				// cancellation must win over a flush that would read as success.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return w.finish(ctx)
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
				return w.finish(ctx)
			}
			switch d.Kind {
			case channels.DeltaText:
				content := w.skipDelivered(d.Content)
				if content == "" {
					continue
				}
				w.timer.Mark(channels.PhaseFirstText)
				w.timer.AddChars(len(content))
				w.mu.Lock()
				w.pending += content
				w.mu.Unlock()
				// The turn's first text opens the stream at once, so the answer
				// shows up while the agent is still writing it; the tick paces
				// the appends that follow. What the agent narrated before the
				// answer has to land above it, and the streamed message exists
				// from its first text on, so the poster cannot catch up later:
				// drain it first, as the terminal flush does.
				if w.streamTS == "" && !w.streamStopped {
					w.drainThreadPosts()
					w.tickFlush(ctx, time.Now())
				}
			case channels.DeltaToolActivity:
				if d.Tool != nil && d.Tool.Kind == channels.ToolCall {
					w.timer.AddToolCall()
				}
				w.renderToolActivity(ctx, d.Tool)
				w.maybeConnectorPrompt(d.Tool)
			case channels.DeltaNarration:
				w.timer.AddChars(len(d.Content))
				w.renderNarration(ctx, d.Content)
			case channels.DeltaPrompt:
				// Recorded before the flush: the stop that closes the stream
				// carries the session's exit status, which for a paused turn is
				// "suspended" — Slack's "waiting for you".
				w.mu.Lock()
				w.promptDelta = &d
				w.mu.Unlock()
				// Flush partial text so far, then hand off to the caller to post
				// the interactive approval prompt. A flush failure here is
				// non-fatal: the pending-task store and the prompt post do not
				// depend on the buffered prose, and failing the turn instead
				// would discard the paused task's only handle, leaving the A2A
				// task unresumable with a dangling tool call.
				if err := w.finalFlush(ctx); err != nil {
					w.logger.Warn("slack: flush at prompt handoff failed, buffered text lost", "error", err)
				}
				return nil
			}

		case now := <-ticker.C:
			w.tickFlush(ctx, now)
		}
	}
}

// maxFlushFailures is the number of attempts a terminal flush gets, and the
// number of consecutive ticker-flush failures after which the retries slow
// down.
const maxFlushFailures = 3

// flushRetryBackoff spaces the ticker flushes once they keep failing, so a
// Slack outage is not hammered every tick for the rest of the turn.
const flushRetryBackoff = 5 * time.Second

// tickFlush runs one paced flush, honouring the backoff a run of failures put
// in place and recording the outcome.
func (w *batchedWriter) tickFlush(ctx context.Context, now time.Time) {
	if now.Before(w.flushRetryAt) {
		return
	}
	if err := w.flush(ctx); err != nil {
		w.noteFlushFailure(now, err)
		return
	}
	w.flushFailures = 0
}

// noteFlushFailure records a failed ticker flush. Text an append did not
// deliver stays pending, so a later flush re-sends it: a Slack failure is never
// fatal to the turn — the agent keeps working, the reply lands once Slack
// accepts it again, and only the final flush's failure is reported (as a
// renderError). Aborting the turn here instead used to cancel the task
// server-side over a rendering problem (klaus-gateway#242).
func (w *batchedWriter) noteFlushFailure(now time.Time, err error) {
	w.flushFailures++
	if w.flushFailures < maxFlushFailures {
		w.logger.Warn("slack: flush failed, retrying next tick", "failures", w.flushFailures, "error", err)
		return
	}
	if w.flushFailures == maxFlushFailures {
		w.logger.Warn("slack: flush keeps failing, retrying until the turn ends", "every", flushRetryBackoff, "error", err)
	}
	w.flushRetryAt = now.Add(flushRetryBackoff)
}

// finish lands the reply once the stream has ended. A final flush that fails
// after its retries is reported as a renderError: the agent completed the turn,
// only its rendering did not.
func (w *batchedWriter) finish(ctx context.Context) error {
	if err := w.finalFlush(ctx); err != nil {
		return &renderError{err: err}
	}
	return nil
}

// renderError is a turn the agent completed whose reply could not be delivered
// to Slack in full. It is distinct from a turn failure so the adapter can tell
// the thread what happened without failing — and cancelling — a turn that
// succeeded server-side.
type renderError struct{ err error }

func (e *renderError) Error() string { return "slack: reply rendering failed: " + e.err.Error() }
func (e *renderError) Unwrap() error { return e.err }

// finalFlush ends the turn's text path (stream done, error, or prompt handoff):
// the text still pending — including the tail the last append held back — rides
// the chat.stopStream that closes the streamed message, so the answer's last
// words and the session's exit status land in one call.
//
// Retried up to maxFlushFailures attempts: no later tick will re-send that
// tail, so a single transient Slack error here would discard it even though the
// turn completed server-side; a short reply that never hits a ticker flush
// would otherwise be killable by one such error.
func (w *batchedWriter) finalFlush(ctx context.Context) error {
	// Every queued in-thread post precedes the reply this flush lands, so drain
	// the poster first to keep the answer the turn's last message.
	w.drainThreadPosts()
	var err error
	for attempt := 1; ; attempt++ {
		if err = w.closeStream(ctx); err == nil {
			return nil
		}
		if errors.Is(err, errStreamLost) || attempt >= maxFlushFailures || ctx.Err() != nil {
			return err
		}
		w.logger.Warn("slack: final flush failed, retrying", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(finalFlushRetryDelay):
		}
	}
}

// tool-activity rendering caps, kept compact so a default-on stream does not
// overwhelm the thread.
const (
	toolArgsMax   = 500
	toolResultMax = 800
	// maxToolEntries bounds tool-activity entries per turn. Entries aggregate
	// into shared activity messages, so the cap guards the per-entry chat.update
	// calls (rate limits) rather than thread flooding; past it, one truncation
	// note is rendered and the rest are silent.
	maxToolEntries = 30
	// maxActivityBlocks bounds the context blocks of one activity message,
	// comfortably under Slack's 50-blocks-per-message limit; further entries
	// roll over into a new activity message.
	maxActivityBlocks = 24
	// maxNarrationMessages bounds narration posts per turn, on its own budget so
	// muted tool activity does not buy extra narration. Past it one truncation
	// note is posted: dropping the agent's prose without saying so is the bug this
	// rendering fixes.
	maxNarrationMessages = 10
)

// narrationLimitNote replaces the narration past the per-turn cap.
const narrationLimitNote = "_…narration limit reached; hiding this turn's remaining step-by-step notes. The answer still follows._"

// toolLimitNote replaces the tool entry past the per-turn cap.
const toolLimitNote = "_…tool-activity limit reached; hiding this turn's remaining tool calls. Details are still on: `/details off` mutes them entirely._"

// renderToolActivity renders a tool call for the thread. At the default level
// (detailsOn) it feeds the live status ticker: one muted line per segment that
// updates in place while the agent works and collapses to a one-line receipt
// when narration closes the segment or the turn ends — no payloads, no
// per-call messages. At detailsFull each
// call (and its result) additionally renders as a context-block entry with its
// JSON payload, aggregated into a shared activity message: the audit view.
// At every level except off the entry is also retained in the adapter's
// per-thread tool log, so the "Inspect agent steps" shortcut can show the
// payloads retroactively; off stays a private mode and records nothing.
// The rendering decision runs here (in run()'s goroutine); the HTTP post is
// handed to an async poster so a slow Slack API does not stall delta draining.
func (w *batchedWriter) renderToolActivity(ctx context.Context, tool *channels.ToolActivity) {
	if w.details == detailsOff || tool == nil {
		return
	}
	displayName, viaMuster := tool.Name, false
	args := tool.Args
	if tool.Kind == channels.ToolCall {
		if inner, innerArgs, ok := unwrapCallTool(tool); ok {
			// Record the call→inner mapping so a detailsFull result (which
			// carries no Args) can resolve the inner name via effectiveToolName,
			// independent of whether connector prompts are enabled.
			w.noteCallToolTarget(tool)
			displayName, viaMuster, args = inner, true, innerArgs
		}
	}

	md, ok := w.toolEntryMarkdown(tool, displayName, viaMuster, args)
	if ok {
		w.recordToolLog(md)
	}

	if w.details != detailsFull {
		if tool.Kind != channels.ToolCall {
			return
		}
		w.recordToolStep(displayName)
		w.kickToolStatus(ctx)
		w.noteDelivered(ctx)
		return
	}
	if !ok {
		return
	}

	w.toolsRendered++
	switch {
	case w.toolsRendered > maxToolEntries+1:
		return // already queued the truncation note
	case w.toolsRendered == maxToolEntries+1:
		md = toolLimitNote
	}

	w.enqueueThreadPost(ctx, threadPost{kind: postToolEntry, md: md})
}

// toolEntryMarkdown renders one tool call or result as the detailsFull entry:
// "🔧 name" with an args code span, or "↳ name result" with a payload preview.
// The result preview is the innermost readable payload (MCP envelopes and
// muster's serialized re-wraps are unwrapped first), with ⚠️ marking a result
// the tool reported as an error. Name, args, and result are agent- and
// MCP-controlled, so everything is escaped for the mrkdwn context block it
// lands in. ok is false when there is nothing to render (a result with no
// preview, or an unknown kind).
func (w *batchedWriter) toolEntryMarkdown(tool *channels.ToolActivity, displayName string, viaMuster bool, args map[string]any) (md string, ok bool) {
	switch tool.Kind {
	case channels.ToolCall:
		md = "🔧 " + toolLabel(displayName)
		if viaMuster {
			md += " (via muster)"
		}
		if summary := compactJSON(args, toolArgsMax); summary != "" {
			md += "\n" + inlineCode(summary)
		}
		return md, true
	case channels.ToolResult:
		resultName := w.effectiveToolName(tool)
		preview, isErr := toolResultPreview(tool.Response, toolResultMax)
		md = "↳ "
		if isErr {
			md += "⚠️ "
		}
		md += toolLabel(resultName) + " result"
		if tool.Name == musterCallToolMetaTool && resultName != tool.Name {
			md += " (via muster)"
		}
		if preview == "" {
			return "", false
		}
		return md + "\n" + inlineCode(preview), true
	default:
		return "", false
	}
}

// recordToolLog retains one rendered entry in the adapter's per-thread tool
// log, opening the turn's log slot on first use. w.threadTS is the thread root,
// the same key the shortcut resolves. Only called from run()'s goroutine, so
// toolLogTurn needs no lock; a resumed run() segment over the same writer (an
// auto-approved prompt) keeps recording into the same turn. Nil adapter means
// a direct-writer test; nothing to record into.
func (w *batchedWriter) recordToolLog(md string) {
	if w.adapter == nil || w.threadTS == "" {
		return
	}
	if w.toolLogTurn == 0 {
		w.toolLogTurn = w.adapter.beginToolLogTurn(w.threadTS)
	}
	w.adapter.appendToolLog(w.threadTS, w.toolLogTurn, md)
}

// recordToolStep counts one tool call into the current segment's ticker state.
func (w *batchedWriter) recordToolStep(displayName string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.toolSteps++
	w.toolCurrent = displayName
	if w.toolCounts == nil {
		w.toolCounts = make(map[string]int)
	}
	if _, seen := w.toolCounts[displayName]; !seen {
		w.toolOrder = append(w.toolOrder, displayName)
	}
	w.toolCounts[displayName]++
}

// kickToolStatus nudges the poster to re-render the status ticker. The kick is
// lossy on purpose (a full queue drops it rather than stalling delta draining):
// the ticker always renders from the exact counters, and every segment's
// receipt renders unconditionally (from the snapshot a closing narration
// carries, or the poster's final pass), so a dropped kick costs at most one
// intermediate refresh, never a step.
func (w *batchedWriter) kickToolStatus(ctx context.Context) {
	if w.threadPosts == nil {
		w.enqueueThreadPost(ctx, threadPost{kind: postToolStatusKick})
		return
	}
	select {
	case w.threadPosts <- threadPost{kind: postToolStatusKick}:
	default:
	}
}

// toolStatusSnapshot returns the live ticker counters for rendering.
func (w *batchedWriter) toolStatusSnapshot() (steps int, current string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.toolSteps, w.toolCurrent
}

// takeToolStatus atomically snapshots and resets the ticker counters, closing
// the current segment. Returns nil when the segment recorded no steps (nothing
// to collapse) — steps the writer was seeded with by a previous process count
// as none until a step of its own joins them: that segment's receipt was
// posted by the process that ran it, a second copy would be noise. The
// snapshot takes ownership of the slices and map: the counters are re-created
// on the next recordToolStep.
func (w *batchedWriter) takeToolStatus() *toolReceipt {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.toolReceiptLocked()
	w.resetToolStatusLocked()
	return r
}

// toolReceipt is the open segment's receipt without closing the segment, for
// the poster's closing pass at the turn's end: the counters stay as they are,
// so what the turn delivered is still recorded after the receipt is posted
// (a restart continues the segment from them). Nil under the same rule as
// takeToolStatus.
func (w *batchedWriter) toolReceipt() *toolReceipt {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.toolReceiptLocked()
	if r != nil {
		r.order, r.counts = slices.Clone(r.order), maps.Clone(r.counts)
	}
	return r
}

func (w *batchedWriter) toolReceiptLocked() *toolReceipt {
	if w.toolSteps <= w.carriedSteps {
		return nil
	}
	return &toolReceipt{steps: w.toolSteps, order: w.toolOrder, counts: w.toolCounts}
}

// resetToolStatus zeroes the ticker counters, the seeded ones included.
func (w *batchedWriter) resetToolStatus() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.resetToolStatusLocked()
}

func (w *batchedWriter) resetToolStatusLocked() {
	w.toolSteps, w.toolCurrent, w.toolOrder, w.toolCounts, w.carriedSteps = 0, "", nil, nil, 0
}

// statusName renders a tool name for the ticker and receipt: plain muted text,
// no code chips. The name is agent-controlled, so mrkdwn control sequences are
// escaped and newlines flattened; emphasis glyphs are cosmetic and left alone.
func statusName(name string) string {
	return strings.ReplaceAll(escapeMrkdwn(name), "\n", " ")
}

// renderToolTicker is the live one-liner shown while the agent works.
func renderToolTicker(steps int, current string) string {
	md := "⏳ " + statusName(current) + "…"
	if steps > 1 {
		md += fmt.Sprintf(" · step %d", steps)
	}
	return md
}

// exitSessionStatus is the state the session lands in when run() returns:
// suspended when the turn paused on a HITL prompt — an approval, an ask_user
// question, a form — which Slack renders as "waiting for you", so the user can
// tell the conversations needing an answer from the finished ones; active on
// every other exit. The answer starts the next turn, which sends processing
// again, so the resume needs nothing of its own.
func (w *batchedWriter) exitSessionStatus() sessionStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.promptDelta != nil {
		return sessionSuspended
	}
	return sessionActive
}

// sessionStatusTimeout bounds one detached agent-session status call.
const sessionStatusTimeout = 10 * time.Second

// sessionStatusIdleAttempts and sessionStatusRetryBackoff bound the retries of
// the exit call (active, or suspended on a prompt pause) on a transport
// failure: a session left in processing keeps spinning for up to an hour and
// nothing but the next turn would repair it, so the exit is worth a couple
// more tries (1s, then 2s apart).
const (
	sessionStatusIdleAttempts = 3
	sessionStatusRetryBackoff = time.Second
)

// setSessionStatus drives the thread's agent session state, which is what
// Slack renders as the native working indicator — in channel threads as well
// as in DMs. It is best-effort and always detached from the turn context: the
// idle status matters most exactly when the turn was cancelled or errored, and
// a status call must never fail a turn.
//
// Only an unsupported-class rejection latches the process-wide downgrade — it
// means this install can never set the status, which no retry fixes within
// this process. A Slack-side rejection of THIS call (the bot not being a
// member of the channel) costs one indicator and is retried by the next turn.
// A transport failure on the exit call is retried a few times here, because
// the working indicator does not clear itself.
func (w *batchedWriter) setSessionStatus(ctx context.Context, status sessionStatus) {
	if w.adapter == nil || w.adapter.sessionStatusUnsupported.Load() {
		return
	}
	// The title rides on the processing call alone: it is the only one that can
	// create the session, and Slack ignores a title on an existing one.
	title := ""
	if status == sessionProcessing {
		title = w.sessionTitle
	}
	base := context.WithoutCancel(ctx)
	var err error
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(base, sessionStatusTimeout)
		err = w.client.setSessionStatus(cctx, w.channel, w.threadTS, status, title)
		cancel()
		if status == sessionProcessing || attempt >= sessionStatusIdleAttempts || !errors.Is(err, errSessionStatusTransient) {
			break
		}
		time.Sleep(sessionStatusRetryBackoff * time.Duration(attempt))
	}
	// Slack gave a verdict against the titled call. The title is decoration;
	// the status is what keeps the indicator honest, so send it once more bare
	// rather than lose this turn's indicator to a title Slack will not take.
	if err != nil && title != "" && !errors.Is(err, errSessionStatusTransient) && !errors.Is(err, errSessionStatusUnsupported) {
		w.logger.Warn("slack: agent session title rejected, setting the status untitled",
			"title_runes", utf8.RuneCountInString(title), "error", err)
		cctx, cancel := context.WithTimeout(base, sessionStatusTimeout)
		err = w.client.setSessionStatus(cctx, w.channel, w.threadTS, status, "")
		cancel()
	}
	switch {
	case err == nil:
	case errors.Is(err, errSessionStatusUnsupported):
		w.adapter.sessionStatusUnsupported.Store(true)
		w.logger.Warn("slack: agent session status unavailable, dropping the native working indicator", "error", err)
	default:
		w.logger.Warn("slack: set agent session status failed", "status", string(status), "error", err)
	}
}

// receiptNameMax bounds how many distinct tool names the receipt lists.
const receiptNameMax = 8

// renderToolReceipt is the one-line summary the ticker collapses into when its
// segment closes (narration posted, or turn ended): step count plus the tools
// used, deduplicated in the segment's first-use order.
func renderToolReceipt(steps int, order []string, counts map[string]int) string {
	label := "steps"
	if steps == 1 {
		label = "step"
	}
	md := fmt.Sprintf("🛠️ %d %s", steps, label)
	for i, name := range order {
		if i == receiptNameMax {
			md += fmt.Sprintf(" · +%d more", len(order)-receiptNameMax)
			break
		}
		md += " · " + statusName(name)
		if n := counts[name]; n > 1 {
			md += fmt.Sprintf(" ×%d", n)
		}
	}
	return md
}

// renderNarration queues the agent's interim narration — the prose it writes
// just before firing tool calls — so it reads in order with the tool posts it
// introduces. Unlike tool activity it ignores the details level: this is the
// agent talking, not tool transparency. It stays out of the main reply buffer,
// whose stream opens on its first text, so the answer keeps landing after the
// narration that led to it.
//
// Narration renders as its own in-thread message, except that at the default
// level a short one folds into the segment's status message (the ticker's
// home) as a muted context block, so a typical narrate-then-call group costs
// one status message instead of two posts. Folding only ever changes WHERE the
// prose renders, never whether: anything not safely short — multi-line, over
// the fold budget, or retractable sign-in prose (retraction deletes whole
// messages by ts, which cannot delete a block inside the shared status
// message) — keeps the own-message rendering.
//
// Narration also closes the live ticker segment: the counters are snapshotted
// and reset here, on run()'s goroutine, exactly at the narration's position in
// the delta stream, and the snapshot travels on the first rendered chunk so
// the poster collapses the segment's status message into a receipt with
// exactly the steps that preceded this narration.
//
// Narration is unbounded agent prose, so it is split like the main reply: Slack
// rejects a whole post whose markdown block exceeds slackMarkdownBlockMax, which
// would drop the text this rendering exists to deliver. Chunks share the
// per-turn budget, so an outsized narration ends in the same limit note.
func (w *batchedWriter) renderNarration(ctx context.Context, text string) {
	scrubbed := strings.TrimSpace(w.scrubLoginURLs(text))
	if scrubbed == "" {
		return
	}
	// Only narration from the point a login challenge was seen is sign-in prose
	// the Connect button contradicts; earlier narration explains tool posts that
	// stay, so it is kept when the prompt takes the turn over.
	retract := len(w.loginURLs) > 0
	fold := w.details == detailsOn && !retract && foldableNarration(scrubbed)
	segmentClosed := false
	for _, md := range splitMarkdown(scrubbed, slackMarkdownBlockMax) {
		w.narrationsRendered++
		switch {
		case w.narrationsRendered > maxNarrationMessages+1:
			return // already queued the truncation note
		case w.narrationsRendered == maxNarrationMessages+1:
			// The note must stay a visible message of its own: folding it into
			// the muted status message would hide the very fact it announces.
			md, fold = narrationLimitNote, false
		}
		p := threadPost{kind: postNarration, md: md, retract: retract, fold: fold}
		if !segmentClosed {
			// Only a narration that actually renders closes the ticker segment,
			// and only its first chunk carries the receipt. Past the narration
			// cap nothing renders, so the segment stays open and its steps roll
			// into the next receipt.
			p.receipt = w.takeToolStatus()
			segmentClosed = true
			w.noteDelivered(ctx)
		}
		w.enqueueThreadPost(ctx, p)
	}
}

// Fold budget for narration rendered inside the status message: one or two
// lines and at most this many runes. Anything larger is real prose, not a
// pre-tool aside, and keeps its own full-weight message. The budget also keeps
// the folded block comfortably inside Slack's 3000-char mrkdwn element cap
// after escaping, and (being far under slackMarkdownBlockMax) guarantees a
// foldable narration is a single chunk.
const (
	foldedNarrationMaxChars = 300
	foldedNarrationMaxLines = 2
)

// foldableNarration reports whether scrubbed narration is short enough to fold
// into the status message.
func foldableNarration(s string) bool {
	return strings.Count(s, "\n") < foldedNarrationMaxLines &&
		utf8.RuneCountInString(s) <= foldedNarrationMaxChars
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
	// The cooldown above is keyed on the raw URL so a re-challenge with the
	// same link stays deduplicated; the posted button carries the decorated one.
	promptURL, connectValue := loginURL, server
	autoResume := false
	if base := w.adapter.PublicBaseURL; base != "" {
		stateID := w.adapter.mintConnectorCompletion(connectorCompletion{slackUser: w.slackUser, server: server, channel: w.channel, threadTS: w.threadTS})
		if decorated, err := decorateConnectorLoginURL(loginURL, base, stateID); err != nil {
			w.logger.Warn("slack: connector login URL decoration failed, posting plain link", "server", server, "error", err)
		} else {
			promptURL, connectValue, autoResume = decorated, stateID, true
		}
	}
	// A button without a post-login redirect is a no-op: no landing fires, so the
	// turn does not auto-resume and the user must sign in and say so. Keep the
	// agent's sign-in narration in that case; only a resumable prompt makes it
	// redundant enough to retract.
	if !autoResume {
		w.connectorManualSignIn = true
	}
	w.adapter.background(func(bg context.Context) {
		ctx, cancel := context.WithTimeout(bg, connectorCheckTimeout)
		defer cancel()
		if err := w.client.postConnectorPrompt(ctx, w.channel, w.threadTS, w.slackUser, server, promptURL, connectValue); err != nil {
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
// MCP-server-controlled text entering an mrkdwn context block: &, <, > must be
// escaped so quoted content cannot trigger notifications, and a backtick or
// newline would break out of the code span and inject markdown into the thread.
func toolLabel(name string) string {
	return "*`" + codeSpanSafe(escapeMrkdwn(name)) + "`*"
}

// inlineCode renders an untrusted single-line payload preview as an mrkdwn code
// span, escaped and sanitised like toolLabel.
func inlineCode(s string) string {
	return "`" + codeSpanSafe(escapeMrkdwn(s)) + "`"
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
		// The challenge often reaches here as an undecoded JSON string, so the
		// whitespace ending the URL is a literal two-character escape (\n, \t)
		// rather than a byte \S+ stops at, and the match runs on into the
		// following prose. Cut at the first such escape before decoding the URL.
		m = cutAtLoginURLTerminator(m)
		m = strings.ReplaceAll(m, jsonEscapedAmp, "&")
		loginURL = validLoginURL(strings.TrimRight(m, ").,]}>\"'"))
	}
	if m := authChallengeServerRe.FindStringSubmatch(output); m != nil {
		server = m[1]
	}
	return server, loginURL
}

// scrubLoginURLs removes the login link the Connect button already carries from
// the agent's prose. The URL is a single-use OAuth authorize link: duplicating
// it in text lets a second click or Slack's unfurl crawler redeem or replay it,
// which the auth server answers by revoking the user's whole token family.
//
// The whole line carrying the link is dropped, not just the URL token: agents
// present the link on its own line (a bullet, an emoji, a markdown link), so
// stripping only the URL leaves dangling scaffolding ("here is the link:" then
// nothing). A single line that just introduces the link (ends with ":") is
// dropped with it. Nothing is left in its place: the Connect button prompt
// already tells the user how to sign in, and any surrounding prose the agent
// wrote (such as "tell me once you're signed in") is kept.
//
// Matching is by authorize-endpoint prefix (everything up to and including
// "?"): the agent re-encodes the query string freely (JSON-escaped ampersands,
// percent-encoded padding), so an exact match cannot be relied on. The prefix
// stops before the first "?", so query re-encoding never affects the match.
func (w *batchedWriter) scrubLoginURLs(text string) string {
	if len(w.loginURLs) == 0 {
		return text
	}
	prefixes := make([]string, 0, len(w.loginURLs))
	for _, loginURL := range w.loginURLs {
		prefix := loginURL
		if i := strings.IndexByte(prefix, '?'); i >= 0 {
			prefix = prefix[:i+1]
		}
		prefixes = append(prefixes, prefix)
	}

	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !lineHasAnyPrefix(line, prefixes) {
			kept = append(kept, line)
			continue
		}
		// Drop a lead-in line whose only purpose was to introduce the link.
		if n := len(kept); n > 0 {
			if prev := strings.TrimSpace(kept[n-1]); strings.HasSuffix(prev, ":") {
				kept = kept[:n-1]
			}
		}
	}
	return collapseBlankLines(strings.Join(kept, "\n"))
}

// lineHasAnyPrefix reports whether line carries any of the authorize-endpoint
// prefixes in any spelling (bare, markdown link, Slack "<url|label>").
func lineHasAnyPrefix(line string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.Contains(line, prefix) {
			return true
		}
	}
	return false
}

// multiBlankLineRe matches a run of two or more blank lines.
var multiBlankLineRe = regexp.MustCompile(`\n[ \t]*\n([ \t]*\n)+`)

// collapseBlankLines trims a run of blank lines left by a removal to a single
// blank, and strips leading and trailing blank lines.
func collapseBlankLines(text string) string {
	return strings.Trim(multiBlankLineRe.ReplaceAllString(text, "\n\n"), "\n")
}

// jsonEscapedAmp is how Go's HTML-safe JSON encoding spells "&" inside a
// string value.
const jsonEscapedAmp = `\u0026`

// loginURLTerminators are the JSON string escapes that end a login URL embedded
// in an undecoded challenge payload: the escape's backslash and letter are
// non-whitespace, so the URL regex swallows them and the prose that follows.
// jsonEscapedAmp is decoded separately and is deliberately not listed.
var loginURLTerminators = []string{`\n`, `\r`, `\t`, `\f`, `\"`}

// cutAtLoginURLTerminator returns s truncated at the first login-URL terminator.
func cutAtLoginURLTerminator(s string) string {
	cut := len(s)
	for _, esc := range loginURLTerminators {
		if i := strings.Index(s, esc); i >= 0 && i < cut {
			cut = i
		}
	}
	return s[:cut]
}

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

// threadPostQueueSize buffers a whole turn's in-thread posts: both producers cap
// themselves before enqueueing, so the send never blocks the delta loop.
const threadPostQueueSize = maxToolEntries + maxNarrationMessages + 2

// enqueueThreadPost buffers a rendered in-thread message for the poster,
// starting it on first use. Called only from run()'s goroutine, so the lazy
// init is race-free.
func (w *batchedWriter) enqueueThreadPost(ctx context.Context, p threadPost) {
	if w.threadPosts == nil {
		w.threadPosts = make(chan threadPost, threadPostQueueSize)
		w.threadPosterDone = make(chan struct{})
		go w.threadPoster(ctx)
	}
	w.threadPosts <- p
}

// activitySegment is the open tool-activity message: consecutive tool entries
// accumulate here as context blocks and land in one message that is updated in
// place, instead of one full-weight message per tool call. dirty marks blocks
// not yet delivered, so a transient post/update failure retries on the next
// flush with everything accumulated since.
type activitySegment struct {
	ts     string
	blocks []any
	dirty  bool
}

// statusMessage is the open status message of the current ticker segment at
// the default details level: the short narration blocks folded above the
// ticker, then the ticker line itself (which the segment's receipt replaces
// when narration closes the segment or the turn ends). dirty marks content not
// yet delivered, so a transient post/update failure retries on the next render
// with everything accumulated since.
type statusMessage struct {
	ts        string
	narration []string // escaped mrkdwn, one context block each, above the ticker
	ticker    string   // current ticker/receipt line; "" until the first tool call renders
	dirty     bool
}

// threadPoster lands queued in-thread items in order: narration as its own
// message (or, when short and its segment's ticker is not live yet, folded
// into the segment's status message), detailsFull tool entries appended to the
// running activity segment, and status kicks refreshed onto the live message
// ticker. A narration message closes the ticker segment: the status message collapses
// into the receipt it carries — at its position, with only that segment's
// counts — and the next tool call opens a fresh status message below the
// narration, so the thread keeps reading in stream order (narration → receipt
// → narration → receipt → …). When the queue closes, the last open segment's
// ticker collapses into its receipt, keeping any folded narration above it.
// Best-effort: a post failure (including a cancelled ctx) is logged and never
// aborts the turn.
func (w *batchedWriter) threadPoster(ctx context.Context) {
	defer close(w.threadPosterDone)
	var seg activitySegment
	var status statusMessage
	// undelivered holds closed status messages whose collapse failed to land, so
	// a transient Slack error never loses a segment's folded narration or
	// receipt counts: later renders keep retrying them.
	var undelivered []statusMessage
	kicked := false
	retryUndelivered := func() {
		kept := undelivered[:0]
		for i := range undelivered {
			w.upsertStatus(ctx, &undelivered[i])
			if undelivered[i].dirty {
				kept = append(kept, undelivered[i])
			}
		}
		undelivered = kept
	}
	// closeSegment collapses the current status message into the closed
	// segment's receipt at its position and opens a fresh status message for
	// the next segment. The exact receipt supersedes any pending ticker
	// refresh, so the kick is consumed with it.
	closeSegment := func(r *toolReceipt) {
		kicked = false
		status.ticker = renderToolReceipt(r.steps, r.order, r.counts)
		status.dirty = true
		w.upsertStatus(ctx, &status)
		if status.dirty {
			undelivered = append(undelivered, status)
		}
		status = statusMessage{}
	}
	// renderStatus delivers the status message when its content changed: a
	// pending kick refreshes the live ticker line from the exact counters, and
	// narration folded since the last render lands with it. Consecutive kicks
	// coalesce into one render, so a burst of tool calls costs a single API
	// call.
	renderStatus := func() {
		retryUndelivered()
		if kicked {
			kicked = false
			if steps, current := w.toolStatusSnapshot(); steps > 0 {
				// The live line stays in the message body on every surface: the
				// agent session status carries no free text, so the native
				// indicator says only that the agent is working and this line is
				// the one that says what it is working on.
				status.ticker = renderToolTicker(steps, current)
				status.dirty = true
			}
		}
		w.upsertStatus(ctx, &status)
	}
	for p := range w.threadPosts {
		// Coalesce everything already queued into this iteration, so a burst of
		// tool entries (a call and its result usually arrive together) costs one
		// API call instead of one per entry.
		for _, q := range w.collectPending(p) {
			switch q.kind {
			case postToolEntry:
				w.appendActivity(ctx, &seg, q.md)
			case postToolStatusKick:
				kicked = true
			default:
				// This narration closes a ticker segment: collapse the status
				// message into the receipt first, so it lands above the
				// narration in stream order.
				if q.receipt != nil {
					closeSegment(q.receipt)
				}
				// Short narration folds into the (fresh, or never-ticking)
				// status message — but only while no ticker line is live
				// (rendered, or pending in this batch): the queue preserves
				// stream order, so a narration seen before any kick really did
				// precede the tools, and folding it above the ticker keeps the
				// reading order exact. Escaped here because it enters an mrkdwn
				// context block, unlike the markdown-block own-message
				// rendering.
				if q.fold && !kicked && status.ticker == "" {
					status.narration = append(status.narration, escapeMrkdwn(q.md))
					status.dirty = true
					continue
				}
				// The open segment and any pending status content precede this
				// narration in the stream, so deliver them first (the segment
				// also starts fresh: appending later entries to the closed one
				// would render them above the narration).
				w.flushActivity(ctx, &seg)
				seg = activitySegment{}
				renderStatus()
				ts, err := w.client.postMarkdown(ctx, w.channel, q.md, w.threadTS)
				if err != nil {
					w.logger.Warn("slack: post in-thread message failed", "error", err)
					continue
				}
				// A ticker starting after this message must not render into a
				// status message sitting above it: close a delivered ticker-less
				// status message so the ticker opens a fresh one below, in
				// stream order. An undelivered one stays open so the next
				// render keeps retrying its folded narration.
				if status.ticker == "" && !status.dirty {
					status = statusMessage{}
				}
				if q.retract {
					w.mu.Lock()
					w.narrationTS = append(w.narrationTS, ts)
					w.mu.Unlock()
				}
			}
		}
		w.flushActivity(ctx, &seg)
		renderStatus()
	}
	// The turn is over (or pausing on a prompt): collapse the last open
	// segment's ticker into its one-line receipt, before the answer lands
	// (finalFlush drains this poster first). A status message whose post failed
	// earlier still gets its receipt and folded narration posted. A cancelled
	// turn (a /stop, the gateway's shutdown) gets its receipt too: the tail runs
	// on a context that outlives the cancellation, briefly, so the ticker is
	// not left frozen mid-step. The counters are read, not taken: the flush
	// that follows records them for a process continuing the turn after a
	// restart, and a run() cycle that follows on this writer resets them.
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), posterTailTimeout)
		defer cancel()
	}
	retryUndelivered()
	if r := w.toolReceipt(); r != nil {
		status.ticker = renderToolReceipt(r.steps, r.order, r.counts)
		status.dirty = true
	}
	w.upsertStatus(ctx, &status)
}

// posterTailTimeout bounds the thread poster's closing receipt once the turn
// context is gone.
const posterTailTimeout = 5 * time.Second

// upsertStatus delivers the status message's blocks — folded narration above
// the ticker line — posting on first use and updating in place afterwards. On
// failure the message stays dirty, so the next render retries (an unposted one
// keeps ts == "" and retries the post).
func (w *batchedWriter) upsertStatus(ctx context.Context, status *statusMessage) {
	if !status.dirty {
		return
	}
	blocks := make([]any, 0, len(status.narration)+1)
	for _, md := range status.narration {
		blocks = append(blocks, contextBlock(md))
	}
	if status.ticker != "" {
		blocks = append(blocks, contextBlock(status.ticker))
	}
	if len(blocks) == 0 {
		status.dirty = false
		return
	}
	if status.ts == "" {
		ts, err := w.client.postActivity(ctx, w.channel, w.threadTS, blocks)
		if err != nil {
			w.logger.Warn("slack: post tool-status message failed", "error", err)
			return
		}
		status.ts = ts
	} else if err := w.client.updateActivity(ctx, w.channel, status.ts, blocks); err != nil {
		w.logger.Warn("slack: update tool-status message failed", "error", err)
		return
	}
	status.dirty = false
}

// collectPending returns p plus every item already queued, without blocking.
func (w *batchedWriter) collectPending(p threadPost) []threadPost {
	batch := []threadPost{p}
	for {
		select {
		case q, ok := <-w.threadPosts:
			if !ok {
				return batch
			}
			batch = append(batch, q)
		default:
			return batch
		}
	}
}

// appendActivity adds one tool entry to the segment, rolling over into a fresh
// message when the per-message block budget is exhausted.
func (w *batchedWriter) appendActivity(ctx context.Context, seg *activitySegment, md string) {
	if len(seg.blocks) >= maxActivityBlocks {
		w.flushActivity(ctx, seg)
		*seg = activitySegment{}
	}
	seg.blocks = append(seg.blocks, contextBlock(md))
	seg.dirty = true
}

// flushActivity delivers the segment's undelivered blocks: the first flush
// posts the activity message, later ones replace it in full (chat.update). On
// failure the segment stays dirty so the next flush retries; the entry caps
// bound how often that can happen.
func (w *batchedWriter) flushActivity(ctx context.Context, seg *activitySegment) {
	if !seg.dirty {
		return
	}
	if seg.ts == "" {
		ts, err := w.client.postActivity(ctx, w.channel, w.threadTS, seg.blocks)
		if err != nil {
			w.logger.Warn("slack: post tool-activity message failed", "error", err)
			return
		}
		seg.ts = ts
	} else if err := w.client.updateActivity(ctx, w.channel, seg.ts, seg.blocks); err != nil {
		w.logger.Warn("slack: update tool-activity message failed", "error", err)
		return
	}
	seg.dirty = false
}

// drainThreadPosts closes the queue and waits for the poster to finish, so every
// in-thread post lands before the main reply. No-op when nothing was queued.
// Idempotent: it clears the queue so a subsequent run() over the same writer (an
// auto-approved resume segment) starts a fresh poster instead of re-closing a
// closed channel.
func (w *batchedWriter) drainThreadPosts() {
	if w.threadPosts == nil {
		return
	}
	close(w.threadPosts)
	<-w.threadPosterDone
	w.threadPosts = nil
	w.threadPosterDone = nil
}

// compactJSON marshals a tool payload to a single readable line: one space after
// each structural ':' and ',' (outside string literals), truncated to max runes.
// Returns "" for an empty or unmarshalable payload.
func compactJSON(v map[string]any, max int) string {
	if len(v) == 0 {
		return ""
	}
	return compactJSONValue(v, max)
}

// compactJSONValue is compactJSON over any JSON value, so unwrapped payloads
// that are arrays render the same single readable line as objects.
func compactJSONValue(v any, max int) string {
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

// maxMCPResultUnwrapDepth bounds the unwrapping of nested serialized MCP
// results. muster's call_tool serializes the inner tool's whole result as the
// outer envelope's text, so real payloads arrive double-wrapped (and the inner
// text is often itself a JSON document); anything deeper is hostile or broken
// input, rendered as-is.
const maxMCPResultUnwrapDepth = 4

// toolResultPreview renders a tool result payload as a single readable line,
// spending the max budget on the innermost actual payload instead of envelope
// boilerplate. An MCP result envelope ({"content": [...], "isError": ...}) or
// kagent's plain-output wrap ({"output": text}) is reduced to its text; text
// that is itself a serialized JSON document (muster's call_tool re-wrap) is
// decoded and unwrapped again up to maxMCPResultUnwrapDepth. The innermost
// payload renders as compact JSON when it is a JSON document and as
// whitespace-collapsed plain text otherwise. isErr reports whether any
// unwrapped envelope flagged the result as an error. Payloads with any other
// shape render unchanged via compactJSON.
func toolResultPreview(resp map[string]any, max int) (preview string, isErr bool) {
	text, isErr, ok := toolResultText(resp)
	if !ok {
		return compactJSON(resp, max), false
	}
	for depth := 0; depth < maxMCPResultUnwrapDepth; depth++ {
		v, isJSON := decodeJSONDocument(text)
		if !isJSON {
			break
		}
		if m, isMap := v.(map[string]any); isMap {
			if inner, innerErr, isEnvelope := toolResultText(m); isEnvelope {
				text = inner
				isErr = isErr || innerErr
				continue
			}
		}
		return compactJSONValue(v, max), isErr
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		// An envelope with no text content (empty content list, non-text items
		// only): fall back to the raw payload so the entry still shows something.
		return compactJSON(resp, max), isErr
	}
	return truncateRunes(text, max), isErr
}

// toolResultText extracts the text a tool result payload carries. ok reports
// whether the payload is a recognized text carrier: an MCP tool-result
// envelope ({"content": [{"type": "text", "text": ...}, ...], "isError": ...}),
// whose text items are joined and whose non-text items render as a [type]
// placeholder, or the ADK/kagent single-key wrap around a plain tool output
// ({"output": text} or {"result": text}, depending on the tool type). Any
// other shape yields ok false so the caller keeps the raw JSON rendering.
func toolResultText(v map[string]any) (text string, isErr, ok bool) {
	items, isEnvelope := v["content"].([]any)
	if !isEnvelope {
		if len(v) == 1 {
			for _, key := range []string{"output", "result"} {
				if out, isText := v[key].(string); isText {
					return out, false, true
				}
			}
		}
		return "", false, false
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		m, isMap := item.(map[string]any)
		if !isMap {
			return "", false, false
		}
		if s, hasText := m["text"].(string); hasText {
			parts = append(parts, s)
			continue
		}
		typ, hasType := m["type"].(string)
		if !hasType {
			return "", false, false
		}
		parts = append(parts, "["+typ+"]")
	}
	isErr, _ = v["isError"].(bool)
	return strings.Join(parts, "\n"), isErr, true
}

// decodeJSONDocument parses text as a JSON object or array. Scalars are
// deliberately not decoded: a bare string or number is already the readable
// payload, and decoding it would strip nothing but its quotes.
func decodeJSONDocument(text string) (v any, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") {
		return nil, false
	}
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return nil, false
	}
	return v, true
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

// wroteContent reports whether a streamed message carries agent text. Used
// after the run loop to decide whether a terminal note may overwrite the
// placeholder, so narration and tool activity deliberately do not count: they
// are separate messages, and a turn that only narrated must still have its
// "thinking" placeholder replaced by the note.
func (w *batchedWriter) wroteContent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.appendedLen > 0
}

// connectorReplyRetractable reports whether this turn's visible reply was only
// a connector sign-in prompt whose button auto-resumes: the ephemeral prompt
// and the post-login resume are the whole exchange, so the streamed agent
// narration is redundant and can be retracted. False when no connector prompt
// was surfaced, or when one was but without a working callback (the user still
// needs the "sign in, then tell me" narration).
func (w *batchedWriter) connectorReplyRetractable() bool {
	return len(w.loginURLs) > 0 && !w.connectorManualSignIn
}

// retractRendered deletes the turn's streamed messages and the narration that
// followed the login challenge, leaving the thread to the connector prompt
// alone. Tool activity stays: it is a transparency record, not sign-in prose
// the button contradicts, and so does the narration that explains it from
// before the challenge. Best-effort: a delete failure is logged, not
// propagated.
// The run loop has finished and drained the poster, so narrationTS is complete;
// it is still read under the lock because the poster wrote it.
func (w *batchedWriter) retractRendered(ctx context.Context) {
	w.mu.Lock()
	narration := w.narrationTS
	w.narrationTS = nil
	w.mu.Unlock()
	for _, ts := range narration {
		if err := w.client.deleteMessage(ctx, w.channel, ts); err != nil {
			w.logger.Warn("slack: retract narration message failed", "ts", ts, "error", err)
		}
	}
	// A message still streaming cannot be retracted, so close it first. The
	// turn is being taken over by the sign-in prompt and is not over, hence
	// processing rather than Slack's active default.
	if w.streamTS != "" {
		if err := w.stopStream(ctx, "", 0, sessionProcessing); err != nil {
			w.logger.Warn("slack: stop the reply stream before retracting it failed", "error", err)
		}
	}
	messages := w.streamMessages
	w.streamMessages = nil
	w.dropStream()
	w.mu.Lock()
	w.appendedLen = 0
	w.mu.Unlock()
	// The row must stop naming text and a message the thread no longer has.
	w.noteDelivered(ctx)
	for _, ts := range messages {
		if err := w.client.deleteMessage(ctx, w.channel, ts); err != nil {
			w.logger.Warn("slack: retract connector reply failed", "ts", ts, "error", err)
		}
	}
}

// flush appends the answer text accumulated since the last one to the turn's
// stream, opening the stream on the first call. The text after the last
// whitespace is held back: an append is final — unlike the chat.update it
// replaces, which re-rendered the whole reply every tick — so a login URL must
// never be sent half-scrubbed and a word never cut in two.
func (w *batchedWriter) flush(ctx context.Context) error {
	if w.streamStopped {
		w.takePending(true) // the text path is closed; drop what is left
		return nil
	}
	if w.streamFailed {
		w.takePending(true)
		return errStreamLost
	}
	raw := w.takePending(false)
	if raw == "" {
		return nil
	}
	if unsent, err := w.appendText(ctx, raw); err != nil {
		w.putBackPending(unsent)
		return err
	}
	return nil
}

// closeStream ends the turn's text path: everything still pending rides the
// chat.stopStream that closes the streamed message, together with the session's
// exit status. Text the open message has no room for goes out as appends first,
// and a turn whose whole answer is still pending opens its stream here. With no
// stream and nothing pending it does nothing, leaving the exit status to run()'s
// own call.
func (w *batchedWriter) closeStream(ctx context.Context) error {
	if w.streamStopped {
		w.takePending(true)
		return nil
	}
	if w.streamFailed {
		w.takePending(true)
		return errStreamLost
	}
	raw := w.takePending(true)
	if raw == "" && w.streamTS == "" {
		return nil
	}
	// Text the open message has no room for goes out as appends, and so does
	// the text of a turn that has no stream yet. A stream adopted from before a
	// restart takes the same route: Slack may have closed it since, and only
	// the append path can move the text onto a stream of its own.
	if raw != "" && (w.streamTS == "" || w.streamAdopted || w.streamed+len(raw) > slackMarkdownBlockMax) {
		unsent, err := w.appendText(ctx, raw)
		if err != nil {
			w.putBackPending(unsent)
			return err
		}
		raw = ""
		if w.streamTS == "" { // the user stopped the stream under us
			return nil
		}
	}
	md := w.scrubLoginURLs(raw)
	if strings.TrimSpace(md) == "" {
		md = ""
	}
	err := w.stopStream(ctx, md, len(raw), w.exitSessionStatus())
	switch {
	case err == nil:
		w.exitStatusSent = !w.streamStopped
		return nil
	case streamGone(err):
		// Nothing is left to close: a stream adopted from before a restart that
		// Slack has closed since, most often. The last text goes back to
		// pending so a retry can give it a stream of its own, and the session's
		// exit status falls back to run()'s own call.
		w.dropStream()
		w.noteDelivered(ctx)
		if raw == "" {
			return nil
		}
		w.putBackPending(raw)
	}
	return err
}

// endStream closes a stream a turn left open. A cancelled turn (a /stop, the
// gateway shutting down) returns without a terminal flush, so without this the
// message would keep animating and the text buffered since the last append
// would be lost. It runs on a context outliving the cancellation, like the
// thread poster's closing pass.
func (w *batchedWriter) endStream(ctx context.Context) {
	if w.streamTS == "" && strings.TrimSpace(w.pendingText()) == "" {
		return
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), streamCloseTimeout)
		defer cancel()
	}
	if err := w.closeStream(ctx); err != nil {
		w.logger.Warn("slack: closing the reply stream failed", "error", err)
	}
}

// appendText delivers raw answer text on the turn's stream and returns the raw
// text it could not deliver, so the caller re-queues exactly what is missing.
// Everything here is measured on the agent's own bytes: that is the unit a
// process continuing the turn after a restart skips, so it has to be the unit
// the pieces are cut in as well.
func (w *batchedWriter) appendText(ctx context.Context, raw string) (unsent string, err error) {
	for raw != "" {
		piece, rest := cutPiece(raw, slackMarkdownBlockMax)
		if err := w.sendPiece(ctx, piece); err != nil {
			return raw, err
		}
		if w.streamStopped {
			return "", nil
		}
		raw = rest
	}
	return "", nil
}

// cutPiece takes the first at most budget bytes of s, cutting at the last
// whitespace inside the budget so a word stays whole — and a login URL, which
// holds no whitespace, is never handed to the scrubbing in halves. A single
// token longer than the budget is cut at the budget, on a rune boundary.
//
// An oversize append is deliberately NOT balanced as Markdown (splitMarkdown):
// on a stream the next piece continues the same message, so a closing fence
// inserted at the cut would break the code block it is inside. A roll-over that
// lands inside a fence is the known cosmetic cost.
func cutPiece(s string, budget int) (piece, rest string) {
	if len(s) <= budget {
		return s, ""
	}
	if i := strings.LastIndexFunc(s[:budget], unicode.IsSpace); i >= 0 {
		_, size := utf8.DecodeRuneInString(s[i:])
		return s[:i+size], s[i+size:]
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], s[cut:]
}

// sendPiece appends one within-cap piece of answer text to the stream, opening
// the stream when none is open and rolling over into a fresh message when the
// open one has no room left. raw is the agent's own text; what Slack receives
// is that text with this turn's login URLs scrubbed out.
func (w *batchedWriter) sendPiece(ctx context.Context, raw string) error {
	md := w.scrubLoginURLs(raw)
	// Scrubbing a pure sign-in passage can empty the piece, and a stream does
	// not open on whitespace alone. Nothing reaches Slack, but the bytes count
	// as delivered all the same: a process continuing the turn would only scrub
	// them away again.
	if md == "" || (w.streamTS == "" && strings.TrimSpace(md) == "") {
		w.noteAppended(len(raw))
		w.noteDelivered(ctx)
		return nil
	}
	// streamed counts the agent's bytes, not the shorter text Slack sees:
	// scrubbing only ever removes, so the count over-estimates and rolls the
	// answer over a little early — the safe side of Slack's per-message cap.
	if w.streamTS != "" && w.streamed+len(raw) > slackMarkdownBlockMax {
		// The answer outgrew one Slack message. The turn is still running, so
		// the stop closing the full message leaves the session processing;
		// Slack's default (active) would clear the working indicator mid-answer.
		if err := w.stopStream(ctx, "", 0, sessionProcessing); err != nil {
			if !streamGone(err) {
				return err
			}
			w.dropStream() // already closed; the rest opens a message of its own
		}
		if w.streamStopped {
			return nil
		}
	}
	if w.streamTS == "" {
		return w.openStream(ctx, raw, md)
	}
	err := w.client.appendStream(ctx, w.channel, w.streamTS, md)
	switch {
	case err == nil:
		w.streamed, w.streamAdopted = w.streamed+len(raw), false
		w.noteAppended(len(raw))
		w.noteDelivered(ctx)
		return nil
	case errors.Is(err, errStreamStoppedByUser):
		w.endStreamQuietly()
		return nil
	case streamGone(err):
		return w.recoverStream(ctx, raw, md)
	}
	return err
}

// openStream posts the turn's streamed message with its first text. In a
// channel Slack requires the recipient the answer is for; the progress
// placeholder the answer supersedes goes once the message exists.
func (w *batchedWriter) openStream(ctx context.Context, raw, md string) error {
	user, team := w.streamRecipient()
	ts, err := w.client.startStream(ctx, w.channel, w.threadTS, md, user, team)
	if err != nil {
		return err
	}
	w.streamTS, w.streamed, w.streamAdopted = ts, len(raw), false
	w.streamMessages = append(w.streamMessages, ts)
	w.noteStream(streamEventStarted)
	w.noteAppended(len(raw))
	w.noteDelivered(ctx)
	w.dropPlaceholder(ctx)
	return nil
}

// stopStream closes the open stream with a last piece of text and the session
// status the stop leaves the thread in. md is what Slack receives and rawLen
// the answer bytes it stands for — the two differ when a login URL was scrubbed
// out of it, and the delivery record counts the answer's own bytes. The status
// is always explicit: Slack defaults it to active, which on an intermediate
// stop would clear the working indicator while the turn keeps running.
func (w *batchedWriter) stopStream(ctx context.Context, md string, rawLen int, status sessionStatus) error {
	err := w.client.stopStream(ctx, w.channel, w.streamTS, md, status)
	switch {
	case err == nil:
	case errors.Is(err, errStreamStoppedByUser):
		w.endStreamQuietly()
		return nil
	default:
		return err
	}
	w.dropStream()
	w.noteStream(streamEventStopped)
	w.noteAppended(rawLen)
	w.noteDelivered(ctx)
	return nil
}

// errStreamLost reports that Slack kept closing the turn's streamed message, so
// the rest of the answer could not be delivered. It travels out as a
// renderError, which is what puts the "reply is incomplete" note in the thread.
var errStreamLost = errors.New("slack: the reply stream keeps closing")

// recoverStream reopens the stream after Slack reported the message no longer
// streaming, and delivers the piece on the new one. One recovery per turn: a
// second says the stream cannot be kept open, and scattering the rest of the
// answer over fresh messages would read worse than stopping there — so the turn
// gives up on the text path and says so in the thread.
func (w *batchedWriter) recoverStream(ctx context.Context, raw, md string) error {
	w.dropStream()
	if w.streamRecovered {
		w.streamFailed = true
		w.logger.Warn("slack: the reply stream keeps closing, the rest of this turn's text is not delivered",
			"channel", w.channel, "thread", w.threadTS)
		return errStreamLost
	}
	w.streamRecovered = true
	w.noteStream(streamEventRecovered)
	w.logger.Warn("slack: the reply stream closed early, opening a new one for the rest",
		"channel", w.channel, "thread", w.threadTS)
	return w.openStream(ctx, raw, md)
}

// endStreamQuietly closes the text path for the rest of the turn after the user
// pressed Stop: Slack has ended the stream, so nothing more is sent on it — no
// retry, no error notice. The stop button's own "Stopped by …" notice is the
// thread's record of what happened.
func (w *batchedWriter) endStreamQuietly() {
	w.dropStream()
	w.streamStopped = true
	w.takePending(true)
	w.noteStream(streamEventStoppedByUser)
}

// dropStream forgets the open stream handle, so the next text of the turn opens
// a message of its own.
func (w *batchedWriter) dropStream() {
	w.streamTS, w.streamed, w.streamAdopted = "", 0, false
}

// resetStream puts the stream state back to its opening shape for a second
// run() cycle over the same writer.
func (w *batchedWriter) resetStream() {
	w.dropStream()
	w.streamRecovered, w.streamStopped, w.streamFailed, w.exitStatusSent = false, false, false, false
}

// streamGone reports whether err says the message is not streaming any more:
// Slack closed it, or it belongs to another app (a handle carried over a
// restart). The handle is dead either way and the rest of the answer needs a
// stream of its own.
func streamGone(err error) bool {
	return errors.Is(err, errStreamNotStreaming) || errors.Is(err, errStreamNotOwned)
}

// streamRecipient names the person a streamed answer is for. Slack requires the
// pair on a stream opened in a channel and refuses it in a DM; an unresolved
// team drops both rather than sending half a pair.
func (w *batchedWriter) streamRecipient() (user, team string) {
	if isDMChannelID(w.channel) || w.slackUser == "" || w.recipientTeam == "" {
		return "", ""
	}
	return w.slackUser, w.recipientTeam
}

// dropPlaceholder removes the text-mode "thinking" placeholder once the answer
// has a streamed message of its own, which is what chat.update used to take
// over in place.
func (w *batchedWriter) dropPlaceholder(ctx context.Context) {
	ts := w.placeholderTS
	if ts == "" {
		return
	}
	w.placeholderTS = ""
	if err := w.client.deleteMessage(ctx, w.channel, ts); err != nil {
		w.logger.Warn("slack: remove the progress placeholder failed", "ts", ts, "error", err)
	}
}

// takePending removes the text to send from the pending buffer. Unless final,
// everything after the last whitespace stays pending: it may be half a word or
// half a login URL, and an appended chunk cannot be taken back.
func (w *batchedWriter) takePending(final bool) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	text := w.pending
	if !final {
		i := strings.LastIndexFunc(text, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		text = text[:i+size]
	}
	w.pending = w.pending[len(text):]
	return text
}

// putBackPending re-queues text an append did not deliver, ahead of whatever
// arrived meanwhile, so the next flush re-sends it in order.
func (w *batchedWriter) putBackPending(text string) {
	if text == "" {
		return
	}
	w.mu.Lock()
	w.pending = text + w.pending
	w.mu.Unlock()
}

// pendingText is the text waiting to be appended.
func (w *batchedWriter) pendingText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending
}

// noteAppended records n bytes of answer text as landed on a stream.
func (w *batchedWriter) noteAppended(n int) {
	w.mu.Lock()
	w.appendedLen += n
	w.mu.Unlock()
}

// StreamRecorder counts the lifecycle events of the streamed replies; the
// observability package implements it with a labelled counter. Nil is fine
// wherever a recorder is optional.
type StreamRecorder interface {
	RecordSlackStream(event string)
}

// Stream lifecycle events counted per turn (see StreamRecorder).
const (
	streamEventStarted       = "started"
	streamEventStopped       = "stopped"
	streamEventStoppedByUser = "stopped_by_user"
	streamEventRecovered     = "recovered"
)

// noteStream counts one stream lifecycle event. A writer with no adapter (a
// direct-writer test) or an adapter with no recorder counts nothing.
func (w *batchedWriter) noteStream(event string) {
	if w.adapter == nil || w.adapter.Streams == nil {
		return
	}
	w.adapter.Streams.RecordSlackStream(event)
}

// slackHTTPClient bounds every Slack Web API call. Without a timeout a
// blackholed connection blocks the calling goroutine indefinitely; some call
// sites hold the per-thread slot while calling (e.g. the users.info lookup
// during dispatch), so an unbounded hang would wedge the thread until process
// restart. Every call is a client span under the turn's, named after the Web
// API method (`slack.chat.update`), so a trace shows where the reply's edits
// sit against the A2A stream.
var slackHTTPClient = &http.Client{Timeout: 30 * time.Second, Transport: tracedTransport(http.DefaultTransport)}

// tracedTransport wraps rt so each request runs as a client span named
// `slack.<method>` under the span on the request's context.
func tracedTransport(rt http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(rt, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
		return "slack." + path.Base(r.URL.Path)
	}))
}

// slackDownloadClient fetches file bytes from url_private. It re-attaches the
// bearer token that net/http strips on a cross-host redirect, but only when the
// redirect target is a Slack host, so the token never leaks to a foreign origin.
// files.slack.com can 302 to a sibling slack.com host; without re-attaching, the
// followed request is unauthenticated and lands on the web sign-in page instead
// of the file. The longer timeout covers large attachments.
var slackDownloadClient = &http.Client{
	Timeout: 60 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("slack download: stopped after 10 redirects")
		}
		if auth := via[0].Header.Get("Authorization"); auth != "" && isSlackHostname(req.URL.Hostname()) {
			req.Header.Set("Authorization", auth)
		}
		return nil
	},
}

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
	// customizeUnsupported, when set, is latched on a missing_scope rejection of
	// a branded post so the adapter skips branding on later posts (the
	// workspace's install predates chat:write.customize). Nil in tests that
	// construct the client directly.
	customizeUnsupported *atomic.Bool
}

// identityRejectedErr reports whether err is Slack rejecting a post because of
// its display identity: missing_scope (no chat:write.customize) or
// invalid_arguments (a username Slack will not accept). Both are retried
// unbranded — branding must cost the label, never the reply.
func identityRejectedErr(err error) bool {
	return hasErrorCode(err, errCodeMissingScope, errCodeInvalidArguments)
}

// noteIdentityRejected logs the unbranded retry and, on missing_scope, latches
// the adapter-wide downgrade so later posts skip the doomed branded attempt.
func (c *slackAPIClient) noteIdentityRejected(err error) {
	if c.logger != nil {
		c.logger.Warn("slack: branded post rejected, retrying under the app identity", "error", err)
	}
	if c.customizeUnsupported != nil && hasErrorCode(err, errCodeMissingScope) {
		c.customizeUnsupported.Store(true)
	}
}

// applyIdentity adds the client's display identity (username/icon_url) via set.
// It is a no-op unless an identity is configured and the method is one that
// honours chat:write.customize (a message the app creates — a post or the start
// of a stream — not an edit or an append). Each field is applied only when
// non-empty: a name without an icon posts under the custom name and the Slack
// app's own icon (Slack keeps the app icon when icon_url is omitted), which is
// what we want while the AgentCard exposes a name but no icon.
func (c *slackAPIClient) applyIdentity(method string, set func(k, v string)) {
	if c.username == "" && c.iconURL == "" {
		return
	}
	if method != methodChatPostMessage && method != "chat.postEphemeral" && method != methodChatStartStream {
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

// viewsOpen opens a modal for the user who produced triggerID. The trigger
// expires 3 seconds after Slack issued it, so callers must not block on slow
// lookups before calling this.
func (c *slackAPIClient) viewsOpen(ctx context.Context, triggerID string, view map[string]any) error {
	_, err := c.postJSON(ctx, "views.open", map[string]any{paramTriggerID: triggerID, paramView: view})
	return err
}

// conversationsJoin joins a public channel (channels:join). Private channels
// refuse it; the caller falls back to asking for an invite.
func (c *slackAPIClient) conversationsJoin(ctx context.Context, channel string) error {
	_, err := c.post(ctx, "conversations.join", url.Values{paramChannel: {channel}})
	return err
}

// threadMessage is one message of a thread as the context read needs it: who
// wrote it and when, and every place Slack puts its words — the plain text, a
// bot's attachments, a Block Kit layout, the names of its files.
type threadMessage struct {
	TS         string `json:"ts"`
	User       string `json:"user"`
	BotID      string `json:"bot_id"`
	Username   string `json:"username"`
	SubType    string `json:"subtype"`
	Text       string `json:"text"`
	BotProfile struct {
		Name string `json:"name"`
	} `json:"bot_profile"`
	Files []struct {
		Name string `json:"name"`
	} `json:"files"`
	Attachments []threadAttachment `json:"attachments"`
	Blocks      []threadBlock      `json:"blocks"`
	// ReplyCount is set on the root message only: the number of replies under
	// it, which is how the picker counts a thread without reading it.
	ReplyCount int `json:"reply_count"`
}

// threadAttachment is the legacy-attachment shape a bot integration (PagerDuty
// and friends) puts its content in when the message's own text is empty.
type threadAttachment struct {
	Title  string `json:"title"`
	Text   string `json:"text"`
	Fields []struct {
		Title string `json:"title"`
		Value string `json:"value"`
	} `json:"fields"`
}

// threadBlock is the part of a Block Kit block that carries words: a section's
// text and fields, a header's text, and the elements of a context or rich_text
// block (whose own elements nest one level further).
type threadBlock struct {
	Type     string            `json:"type"`
	Text     *threadBlockText  `json:"text"`
	Fields   []threadBlockText `json:"fields"`
	Elements []threadBlockElem `json:"elements"`
}

type threadBlockText struct {
	Text string `json:"text"`
}

type threadBlockElem struct {
	Type     string            `json:"type"`
	Text     blockString       `json:"text"`
	Elements []threadBlockElem `json:"elements"`
}

// blockString is a Block Kit element's "text": a plain string in a rich_text
// run, a text object ({"type":"plain_text","text":"Reopen"}) in a button or a
// context element — PagerDuty's alert posts carry both in one message. Either
// shape decodes to the words; anything else decodes to "", so one unusual
// element never fails the read of a whole thread.
type blockString string

func (s *blockString) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = blockString(str)
		return nil
	}
	var obj struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		*s = blockString(obj.Text)
		return nil
	}
	*s = ""
	return nil
}

// threadRepliesPageSize is the page size of a context read. The shared call
// helper reads at most 1 MiB of a response and cannot tell a truncated body
// from a complete one, and one alert message with its blocks and attachments
// is easily several kilobytes, so a page stays well under that ceiling: a page
// too large would decode to nothing and lose the whole transcript.
const threadRepliesPageSize = 50

// threadContextMaxPages bounds what one transcript may cost Slack: ten pages,
// about 500 messages. A thread longer than that is read as far as this and
// handed over labelled as a partial read, never as its newest messages. It
// also bounds the read's memory by construction, so nothing needs to be
// discarded while paging — the character cap that shapes the transcript needs
// the rendered lines, which only the renderer has.
const threadContextMaxPages = 10

// threadRead is one context read: the messages it paged over (oldest first,
// the thread's root first of all), how many messages the thread holds
// according to the root's reply count (0 when Slack did not report one),
// whether the read reached the end of the thread, and — when a page after the
// first one failed — what Slack said, so the log can tell a read the page
// bound stopped from one a rate limit or an outage cut short.
type threadRead struct {
	Messages []threadMessage
	Total    int
	Complete bool
	Err      error
}

// threadReplies reads a thread through conversations.replies, oldest first,
// following the cursor across pages. It is NOT a routing-state read — the
// thread record in the store stays the only carrier of a thread's agent,
// initiator and grants; this reads the words people wrote, once, to hand them
// to the agent a conversation pulls into the thread (see threadcontext.go). A
// page that fails after the first one yields what was read so far, marked
// incomplete: part of a thread is worth more to the agent than none, as long
// as the transcript does not claim to be the newest part.
func (c *slackAPIClient) threadReplies(ctx context.Context, channel, threadTS string) (threadRead, error) {
	read := threadRead{}
	cursor := ""
	for page := 0; page < threadContextMaxPages; page++ {
		params := url.Values{
			paramChannel: {channel},
			paramTS:      {threadTS},
			paramLimit:   {strconv.Itoa(threadRepliesPageSize)},
		}
		if cursor != "" {
			params.Set(paramCursor, cursor)
		}
		result, err := c.repliesPage(ctx, params)
		if err != nil {
			if len(read.Messages) == 0 {
				return threadRead{}, err
			}
			read.Err = err
			return read, nil
		}
		read.Messages = append(read.Messages, result.Messages...)
		// The root carries the thread's reply count, which is how a partial
		// read can say how much of the thread it did not reach.
		if page == 0 && len(result.Messages) > 0 && result.Messages[0].ReplyCount > 0 {
			read.Total = result.Messages[0].ReplyCount + 1
		}
		cursor = result.ResponseMetadata.NextCursor
		if cursor == "" {
			read.Complete = true
			break
		}
	}
	return read, nil
}

// repliesResponse is one conversations.replies page.
type repliesResponse struct {
	OK               bool            `json:"ok"`
	Err              string          `json:"error,omitempty"`
	Messages         []threadMessage `json:"messages"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// repliesPage runs one conversations.replies call and decodes it. A Slack
// refusal is returned as an apiError so the caller can name its code
// (missing_scope, not_in_channel, …) to the person.
func (c *slackAPIClient) repliesPage(ctx context.Context, params url.Values) (repliesResponse, error) {
	body, err := c.call(ctx, "conversations.replies", "application/x-www-form-urlencoded", params.Encode())
	if err != nil {
		return repliesResponse{}, err
	}
	var result repliesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		// A type mismatch is one field of one message in a shape this decoder
		// did not expect (a PagerDuty button's label was the first); encoding/json
		// skips that value, fills every other field and reports the mismatch at
		// the end, so the page is usable and the thread is not lost for a field
		// the transcript may not even want. Anything else is a broken body.
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return repliesResponse{}, fmt.Errorf("slack conversations.replies: decode: %w", err)
		}
	}
	if !result.OK {
		return repliesResponse{}, &apiError{method: "conversations.replies", code: result.Err}
	}
	return result, nil
}

// respondToURL posts an ephemeral text reply through a slash command's
// response_url (usable five times within 30 minutes). The URL is a Slack
// webhook, not a Web API method: no bot token, no envelope.
func (c *slackAPIClient) respondToURL(ctx context.Context, responseURL, text string) error {
	if responseURL == "" {
		return errors.New("slack: no response_url")
	}
	data, err := json.Marshal(map[string]any{"response_type": "ephemeral", paramText: text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := slackHTTPClient.Do(req) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("slack response_url: status %d", resp.StatusCode)
	}
	return nil
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
		return "", &apiError{method: "users.info", code: result.Err}
	}
	return result.User.Profile.Email, nil
}

// lookupUserDisplayName returns the human-facing name from the user's Slack
// profile, preferring the display name and falling back to the real name. Used
// to name the bot itself in help text so the example matches what people see in
// Slack. Returns "" (no error) when the profile carries no name.
func (c *slackAPIClient) lookupUserDisplayName(ctx context.Context, userID string) (string, error) {
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
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack users.info: decode: %w", err)
	}
	if !result.OK {
		return "", &apiError{method: "users.info", code: result.Err}
	}
	if result.User.Profile.DisplayName != "" {
		return result.User.Profile.DisplayName, nil
	}
	return result.User.Profile.RealName, nil
}

// authTest returns the bot's own Slack user ID, username and team via
// auth.test, used to recognise the bot's own channel-join event, to name the
// bot in help text, and to name the recipient's workspace on a streamed reply
// in a channel. The username may be empty when Slack omits it.
func (c *slackAPIClient) authTest(ctx context.Context) (userID, username, teamID string, err error) {
	body, err := c.call(ctx, "auth.test", "application/x-www-form-urlencoded", "")
	if err != nil {
		return "", "", "", err
	}

	var result struct {
		OK     bool   `json:"ok"`
		Err    string `json:"error,omitempty"`
		UserID string `json:"user_id"`
		User   string `json:"user"`
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", "", fmt.Errorf("slack auth.test: decode: %w", err)
	}
	if !result.OK {
		return "", "", "", fmt.Errorf("slack auth.test: %s", result.Err)
	}
	return result.UserID, result.User, result.TeamID, nil
}

// errReactionsUnsupported reports that the bot cannot manage reactions (the
// reactions:write scope is missing, or the token type disallows it), so the
// caller should fall back to text-based progress.
var errReactionsUnsupported = errors.New("slack: reactions unsupported")

// sessionStatus is a Slack agent session's lifecycle state, the enum
// agents.sessions.setStatus accepts. processing shows the native working
// indicator (and the stop button), active is idle/ready, suspended waits on
// the user, and closed ends the session.
type sessionStatus string

const (
	sessionProcessing sessionStatus = "processing"
	sessionActive     sessionStatus = "active"
	sessionSuspended  sessionStatus = "suspended"
	sessionClosed     sessionStatus = "closed"
)

// errSessionStatusUnsupported reports that this install can never set the
// agent session status (the chat:write scope is missing, the token type
// disallows it, or agent messaging is disabled for the workspace), so the
// caller should latch the process-wide downgrade. not_authorized is
// deliberately NOT in this class: it says the bot is not a member of THIS
// channel, which tells us nothing about the next one.
var errSessionStatusUnsupported = errors.New("slack: agent session status unsupported")

// errSessionStatusTransient marks a status call that never got a Slack API
// verdict (network error, timeout, non-2xx, rate-limit budget exhausted). It is
// the only class the idle call retries: a Slack-side rejection would repeat.
var errSessionStatusTransient = errors.New("slack: agent session status transport failure")

// sessionStatusResponse is the agents.sessions.setStatus reply. The warning
// fields are decoded rather than dropped: until the app subscribes to
// agent_session_stopped Slack answers an otherwise successful call with a
// warning, which is informational and must not read as a failure.
type sessionStatusResponse struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error,omitempty"`
	Warning          string `json:"warning,omitempty"`
	ResponseMetadata struct {
		Warnings []string `json:"warnings,omitempty"`
	} `json:"response_metadata,omitempty"`
}

// sessionTitleMax is Slack's cap on an agent session title, in characters.
const sessionTitleMax = 200

// storeSessionTitle parks the title threadID's agent session is created with,
// derived from the conversation's opening message. Only that message's turn
// can create the session — Slack applies a title on creation and ignores it
// afterwards, which is also what keeps a title a user edited by hand from
// being overwritten — so a reply, and a button resume, park nothing.
//
// The title is parked on the thread instead of handed to the turn because the
// opening message does not always run as a turn on its first dispatch: a
// sender who has not signed in yet has it held and replayed, and the replay
// re-enters dispatch with the conversation already bound, where it no longer
// reads as the opener. The turn that finally sends the processing status
// takes the title. An empty title (a bare command, an upload with no caption)
// parks nothing, so Slack names that session itself.
func (a *Adapter) storeSessionTitle(threadID, title string) {
	if title == "" {
		return
	}
	now := time.Now()
	a.sessionTitleMu.Lock()
	defer a.sessionTitleMu.Unlock()
	if a.sessionTitles == nil {
		a.sessionTitles = make(map[string]ttlEntry[string])
	}
	sweepExpired(a.sessionTitles, now)
	a.sessionTitles[threadID] = ttlEntry[string]{value: title, expires: now.Add(threadStateTTL)}
}

// takeSessionTitle returns and clears threadID's parked session title, or ""
// when this turn is not the conversation's first.
func (a *Adapter) takeSessionTitle(threadID string) string {
	a.sessionTitleMu.Lock()
	defer a.sessionTitleMu.Unlock()
	entry, ok := a.sessionTitles[threadID]
	if !ok {
		return ""
	}
	delete(a.sessionTitles, threadID)
	if time.Now().After(entry.expires) {
		return ""
	}
	return entry.value
}

// sessionTitleFrom derives a session title from the first human message of a
// thread — the line the Messages tab timeline lists the conversation under,
// where an untitled session reads as nothing at all. It also names the thread's
// kagent conversation, so both surfaces list the thread under the same line.
//
// The command scaffolding a user types to address the bot says nothing about
// the conversation, so the mention and a leading slash verb (the /agent
// selector, or any other command-shaped verb) are dropped and only the
// question survives; channels.TitleFrom does the rest. Returns "" when nothing
// survives, in which case no title is sent and Slack names the session itself.
func sessionTitleFrom(text string) string {
	s := StripMention(strings.TrimSpace(text))
	if cmd := parseCommand(s); cmd != nil && commandShapeRe.MatchString(cmd.Name) {
		if cmd.Name == cmdAgent {
			_, _, s = splitAgentCommand(s)
		} else {
			s = strings.Join(cmd.Args, " ")
		}
	}
	return channels.TitleFrom(s, sessionTitleMax)
}

// setSessionStatus sets the thread's agent session status, creating the
// session if it does not exist yet. channel_id and thread_ts are always sent:
// the session is thread-based on every surface we serve. Unlike the legacy
// assistant status this never clears itself when the app posts, so the caller
// owns sending the idle state on every exit path.
//
// title names the session in the Messages tab timeline. Slack applies it only
// when this call CREATES the session, so it is sent on the turn that opens the
// conversation and ignored (harmlessly) on any later one; a session a user
// renamed by hand therefore keeps its name. An empty title is omitted rather
// than sent blank.
//
// The call goes out unbranded on purpose: the display-identity fields would
// need chat:write.customize, and its missing_scope rejection is
// indistinguishable from the one that latches this method off for the whole
// process.
func (c *slackAPIClient) setSessionStatus(ctx context.Context, channelID, threadTS string, status sessionStatus, title string) error {
	params := map[string]any{
		paramChannelID: channelID,
		paramThreadTS:  threadTS,
		paramStatus:    string(status),
	}
	if title != "" {
		params[paramTitle] = title
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("slack %s: marshal: %w", methodSetSessionStatus, err)
	}
	body, err := c.call(ctx, methodSetSessionStatus, "application/json; charset=utf-8", string(payload))
	if err != nil {
		return fmt.Errorf("%w: %w", errSessionStatusTransient, err)
	}
	var result sessionStatusResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("slack %s: decode response: %w", methodSetSessionStatus, err)
	}
	if !result.OK {
		return sessionStatusErr(&apiError{method: methodSetSessionStatus, code: result.Error})
	}
	c.noteSessionStatusWarnings(result)
	return nil
}

// sessionStatusErr maps the rejections that mean the install can never set the
// status to errSessionStatusUnsupported (the latch signal) and surfaces
// everything else as-is.
func sessionStatusErr(err error) error {
	if hasErrorCode(err, errCodeMissingScope, errCodeNotAllowedTokenType, errCodeFeatureDisabled) {
		return errSessionStatusUnsupported
	}
	return err
}

// sessionStatusWarnOnce keeps the session warning to one debug line per
// process: Slack repeats it on every call until the app subscribes to
// agent_session_stopped, so logging each one would be pure noise.
var sessionStatusWarnOnce sync.Once

func (c *slackAPIClient) noteSessionStatusWarnings(r sessionStatusResponse) {
	if c.logger == nil {
		return
	}
	warnings := r.ResponseMetadata.Warnings
	if r.Warning != "" {
		warnings = append(warnings, r.Warning)
	}
	if len(warnings) == 0 {
		return
	}
	sessionStatusWarnOnce.Do(func() {
		c.logger.Debug("slack: agent session status warning", "warnings", warnings)
	})
}

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
	if hasErrorCode(err, errCodeMissingScope, errCodeNotAllowedTokenType) {
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

// fallbackText is the top-level text of a markdown-block message: the
// notification and accessibility fallback. It is mrkdwn-parsed by Slack, so
// agent output is escaped there even though the markdown block itself must not
// be, and it is cut to slackFallbackTextMax so chat.update never refuses the
// message for it. The cut never leaves a half entity (`&amp;` cut to `&am`).
func fallbackText(md string) string {
	s := escapeMrkdwn(md)
	if utf8.RuneCountInString(s) <= slackFallbackTextMax {
		return s
	}
	cut := string([]rune(s)[:slackFallbackTextMax-1])
	if i := strings.LastIndexByte(cut, '&'); i >= 0 && !strings.Contains(cut[i:], ";") {
		cut = cut[:i]
	}
	return cut + "…"
}

// postMarkdown posts a new in-thread message rendered as a markdown block.
func (c *slackAPIClient) postMarkdown(ctx context.Context, channel, md, threadTS string) (string, error) {
	body := map[string]any{
		paramChannel: channel,
		paramText:    fallbackText(md),
		paramBlocks:  markdownBlocks(md),
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// activityFallbackText is the notification/accessibility fallback of a
// tool-activity message; the context blocks carry the real content.
const activityFallbackText = "Tool activity"

// contextBlock wraps one rendered tool entry in a Block Kit context block,
// which Slack renders as small muted text — visually subordinate to the
// agent's prose, which is what tool transparency should be.
func contextBlock(md string) map[string]any {
	return map[string]any{
		bkType:     bkContext,
		bkElements: []any{map[string]any{bkType: bkMrkdwn, bkText: md}},
	}
}

// postActivity posts a new in-thread tool-activity message carrying blocks.
func (c *slackAPIClient) postActivity(ctx context.Context, channel, threadTS string, blocks []any) (string, error) {
	body := map[string]any{
		paramChannel: channel,
		paramText:    activityFallbackText,
		paramBlocks:  blocks,
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// updateActivity replaces a tool-activity message's blocks in full; chat.update
// carries no partial-append, so every delivered entry is resent.
func (c *slackAPIClient) updateActivity(ctx context.Context, channel, ts string, blocks []any) error {
	_, err := c.postJSON(ctx, "chat.update", map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramText:    activityFallbackText,
		paramBlocks:  blocks,
	})
	return err
}

// chatUpdateMarkdown replaces a message's content with a markdown block.
func (c *slackAPIClient) chatUpdateMarkdown(ctx context.Context, channel, ts, md string) error {
	body := map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramText:    fallbackText(md),
		paramBlocks:  markdownBlocks(md),
	}
	_, err := c.postJSON(ctx, "chat.update", body)
	return err
}

// errStreamStoppedByUser reports that the user pressed the Stop button: Slack
// has already closed the stream, so the text path ends there, quietly.
var errStreamStoppedByUser = errors.New("slack: stream stopped by the user")

// errStreamNotStreaming reports that the message is no longer in a streaming
// state, so the rest of the text needs a stream of its own.
var errStreamNotStreaming = errors.New("slack: message not in a streaming state")

// errStreamNotOwned reports a stream call against a message this app did not
// post.
var errStreamNotOwned = errors.New("slack: message not owned by the app")

// startStream opens a streaming message in the thread carrying its first text
// and returns its ts. recipientUser and recipientTeam name the person the
// answer is for: Slack requires both when the stream is in a channel and
// refuses them in a DM, so the caller passes them only for a channel. The
// display identity rides along as it does on a new post, including the
// unbranded retry when the customize scope is missing (see postJSON).
func (c *slackAPIClient) startStream(ctx context.Context, channel, threadTS, md, recipientUser, recipientTeam string) (string, error) {
	body := map[string]any{
		paramChannel:      channel,
		paramMarkdownText: md,
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	if recipientUser != "" && recipientTeam != "" {
		body[paramRecipientUserID] = recipientUser
		body[paramRecipientTeamID] = recipientTeam
	}
	ts, err := c.postJSON(ctx, methodChatStartStream, body)
	return ts, streamErr(err)
}

// appendStream adds text to an open stream. Unlike chat.update it carries only
// what is new, so a long answer costs one small call per tick.
func (c *slackAPIClient) appendStream(ctx context.Context, channel, ts, md string) error {
	_, err := c.postJSON(ctx, methodChatAppendStream, map[string]any{
		paramChannel:      channel,
		paramTS:           ts,
		paramMarkdownText: md,
	})
	return streamErr(err)
}

// stopStream closes a stream, optionally with a last piece of text, and sets
// the thread's agent session status in the same call. status is always sent:
// Slack defaults the field to active, which would clear the working indicator
// on a stop that only rolls the answer over into a new message.
func (c *slackAPIClient) stopStream(ctx context.Context, channel, ts, md string, status sessionStatus) error {
	body := map[string]any{
		paramChannel:       channel,
		paramTS:            ts,
		paramSessionStatus: string(status),
	}
	if md != "" {
		body[paramMarkdownText] = md
	}
	_, err := c.postJSON(ctx, methodChatStopStream, body)
	return streamErr(err)
}

// streamErr maps the streaming methods' documented rejections to the sentinels
// the writer reacts to, keeping Slack's own error for the log.
func streamErr(err error) error {
	switch {
	case err == nil:
		return nil
	case hasErrorCode(err, errCodeStoppedByUser):
		return fmt.Errorf("%w: %w", errStreamStoppedByUser, err)
	case hasErrorCode(err, errCodeNotInStreamingState):
		return fmt.Errorf("%w: %w", errStreamNotStreaming, err)
	case hasErrorCode(err, errCodeMsgNotOwned):
		return fmt.Errorf("%w: %w", errStreamNotOwned, err)
	}
	return err
}

// deleteMessage removes a message the gateway posted (chat.delete). Used to
// retract the streamed agent bubble when a turn's only reply was a connector
// sign-in prompt whose button carries a working auto-resume callback.
func (c *slackAPIClient) deleteMessage(ctx context.Context, channel, ts string) error {
	_, err := c.postJSON(ctx, "chat.delete", map[string]any{
		paramChannel: channel,
		paramTS:      ts,
	})
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

// signInPromptText is the sign-in prompt's body. It names no one: the prompt
// reaches its user through the message's audience (an ephemeral in a channel,
// a DM thread otherwise), never through an @-mention a whole thread can read
// (klaus-gateway#185).
const signInPromptText = "Sign in so I can act as you. Until you do, I can't run tools on your behalf."

// signInPromptBody builds the sign-in prompt's Slack post body: a section with
// the prompt text plus a "Sign in" URL button opening linkURL. A non-empty lead
// is put on its own line above the prompt text.
func signInPromptBody(channel, threadID, linkURL, lead string) map[string]any {
	text := signInPromptText
	if lead != "" {
		text = lead + "\n" + text
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
	return body
}

// postSignInPrompt posts the sign-in prompt as a real threaded message and
// returns its ts. It is the DM form of the prompt: a DM thread has one reader,
// so nothing is hidden by making it ephemeral, and only thread replies render
// in the assistant pane. The returned ts lets the prompt be rewritten in place
// once the link completes.
func (c *slackAPIClient) postSignInPrompt(ctx context.Context, channel, threadID, linkURL string) (string, error) {
	return c.postJSON(ctx, methodChatPostMessage, signInPromptBody(channel, threadID, linkURL, ""))
}

// postSignInPromptEphemeral posts the sign-in prompt visible to user only. It
// is the channel form: the link is minted for one identity, so a thread full
// of bystanders must not see it (klaus-gateway#185). An ephemeral has no
// addressable ts, so it cannot be rewritten later; the caller confirms the
// completed link with a fresh ephemeral instead. Slack only surfaces a
// thread-scoped ephemeral in a thread that already shows a message, which is
// why the caller anchors a fresh mention first (klaus-gateway#156). lead is put
// above the prompt text when this prompt replaces one whose link expired.
func (c *slackAPIClient) postSignInPromptEphemeral(ctx context.Context, channel, threadID, user, linkURL, lead string) error {
	body := signInPromptBody(channel, threadID, linkURL, lead)
	body[paramUser] = user
	_, err := c.postJSON(ctx, "chat.postEphemeral", body)
	return err
}

// slackSectionTextMax is Slack's limit on a section block's text object; a
// longer text gets the whole message rejected with invalid_blocks.
const slackSectionTextMax = 3000

// postConnectorPrompt posts the agent's Connect prompt: an ephemeral offering
// to connect a muster backend the agent cannot use for the user yet, with a
// "Not now" dismissal (the prompt cooldown then holds it back).
func (c *slackAPIClient) postConnectorPrompt(ctx context.Context, channel, threadID, user, server, loginURL, connectValue string) error {
	text := fmt.Sprintf("The agent can't use *%s* for you yet. Connect your account once so those tools work.", escapeMrkdwn(server))
	return c.postConnectPrompt(ctx, channel, threadID, user, text, server, loginURL, connectValue, true)
}

// postConnectPrompt posts an ephemeral (target-user-only) Block Kit message
// with a "Connect <server>" URL button opening loginURL under text, and a
// "Not now" button when dismiss is set. When threadID is set the prompt is
// posted in-thread. connectValue is the Connect button's value: the
// completion-state ID when the login URL carries a post-login redirect, else
// the server name (the click stays a no-op then).
func (c *slackAPIClient) postConnectPrompt(ctx context.Context, channel, threadID, user, text, server, loginURL, connectValue string, dismiss bool) error {
	elements := []any{
		map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: truncateButtonLabel("Connect " + server)},
			bkStyle:    bkPrimary,
			bkActionID: connectorConnect,
			bkValue:    connectValue,
			bkURL:      loginURL,
		},
	}
	if dismiss {
		elements = append(elements, map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: "Not now"},
			bkActionID: connectorDismiss,
			bkValue:    server,
		})
	}
	body := map[string]any{
		paramChannel: channel,
		paramUser:    user,
		paramText:    text,
		paramBlocks: []any{
			map[string]any{
				bkType: bkSection,
				bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
			},
			map[string]any{bkType: bkActions, bkElements: elements},
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
var interactionHTTPClient = &http.Client{Timeout: 10 * time.Second, Transport: tracedTransport(http.DefaultTransport)}

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
	return c.chatUpdate(ctx, channel, ts, text, []any{})
}

// chatUpdate rewrites a message to the given blocks, text being the
// notification fallback.
func (c *slackAPIClient) chatUpdate(ctx context.Context, channel, ts, text string, blocks []any) error {
	body := map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramText:    text,
		paramBlocks:  blocks,
	}
	_, err := c.postJSON(ctx, "chat.update", body)
	return err
}

func (c *slackAPIClient) postJSON(ctx context.Context, method string, body any) (string, error) {
	// The identity fields go onto a clone so the caller's map stays untouched —
	// which also keeps the original available for the unbranded retry below.
	m, isMap := body.(map[string]any)
	build := func(withIdentity bool) (any, bool) {
		if !isMap {
			return body, false
		}
		cloned := maps.Clone(m)
		branded := false
		if withIdentity {
			c.applyIdentity(method, func(k, v string) {
				if _, exists := cloned[k]; !exists {
					cloned[k] = v
					branded = true
				}
			})
		}
		if method == methodChatPostMessage {
			// Bot posts relay agent- and tool-controlled links; an unfurl has
			// Slack's crawler fetch them, which for single-use auth links can
			// trip the auth server's replay detection.
			cloned[paramUnfurlLinks] = false
			cloned[paramUnfurlMedia] = false
		}
		return cloned, branded
	}
	payload, branded := build(true)
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("slack %s: marshal: %w", method, err)
	}
	ts, err := c.send(ctx, method, "application/json; charset=utf-8", string(data))
	if branded && identityRejectedErr(err) {
		c.noteIdentityRejected(err)
		payload, _ = build(false)
		if data, merr := json.Marshal(payload); merr == nil {
			return c.send(ctx, method, "application/json; charset=utf-8", string(data))
		}
	}
	return ts, err
}

type slackResponse struct {
	OK    bool   `json:"ok"`
	Ts    string `json:"ts"`
	Error string `json:"error,omitempty"`
}

func (c *slackAPIClient) post(ctx context.Context, method string, params url.Values) (string, error) {
	branded := false
	c.applyIdentity(method, func(k, v string) {
		params.Set(k, v)
		branded = true
	})
	if method == methodChatPostMessage {
		params.Set(paramUnfurlLinks, "false")
		params.Set(paramUnfurlMedia, "false")
	}
	ts, err := c.send(ctx, method, "application/x-www-form-urlencoded", params.Encode())
	if branded && identityRejectedErr(err) {
		c.noteIdentityRejected(err)
		params.Del(paramUsername)
		params.Del(paramIconURL)
		return c.send(ctx, method, "application/x-www-form-urlencoded", params.Encode())
	}
	return ts, err
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
		return "", &apiError{method: method, code: result.Error}
	}
	return result.Ts, nil
}

// apiError is a Slack Web API refusal (`ok: false`) carrying the error code
// Slack named, so callers react to a code (msg_too_long, missing_scope) rather
// than to an error string.
type apiError struct {
	method string
	code   string
}

func (e *apiError) Error() string { return fmt.Sprintf("slack %s: %s", e.method, e.code) }

// apiErrorCode returns the Slack error code err carries, or "" when err is not
// a Slack API refusal.
func apiErrorCode(err error) string {
	var e *apiError
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}

// hasErrorCode reports whether err is a Slack API refusal with one of codes.
func hasErrorCode(err error, codes ...string) bool {
	code := apiErrorCode(err)
	return code != "" && slices.Contains(codes, code)
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
// small margin as an out-of-memory guard against a mismatched or hostile
// response, and to maxAttachmentDownload overall so an honestly declared huge
// file is refused up front instead of buffered whole.
func (c *slackAPIClient) downloadFile(ctx context.Context, fileURL, declaredType string, sizeHint int) ([]byte, error) {
	if sizeHint > maxAttachmentDownload {
		return nil, fmt.Errorf("slack download: declared size %d exceeds the %d-byte attachment ceiling", sizeHint, maxAttachmentDownload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("slack download: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)
	// Signal a raw-file (not browser) fetch. A request Slack reads as a browser
	// navigation is bounced to the web sign-in page instead of the bytes.
	req.Header.Set("Accept", "*/*")

	resp, err := slackDownloadClient.Do(req) //nolint:gosec
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
// Content-Type variants Slack uses for it. Every marker is Slack-specific: a
// bare "signin" substring would misclassify a user's own HTML upload that
// merely links to its own sign-in route.
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
	return strings.Contains(lower, "slack-edge.com") ||
		strings.Contains(lower, "data-primer") ||
		strings.Contains(lower, "slack.com/signin") ||
		strings.Contains(lower, "sign in to slack")
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
