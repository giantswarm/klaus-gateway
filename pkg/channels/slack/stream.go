package slack

import (
	"bytes"
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
	// streamCallTimeout bounds one write to the turn's streamed message. Those
	// writes are detached from the turn's cancellation (streamCallCtx), so a
	// cancelled turn waits for the one in flight before it exits: shorter than
	// the 30 s of slackHTTPClient, because that wait is what a /stop costs.
	streamCallTimeout = 10 * time.Second
	slackAPIBase      = "https://slack.com/api"
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
	// of one streamed message, Slack's 12 000-char limit. A narration passage
	// is split under it by splitMarkdown, fence close and reopen included; a
	// streamed answer is cut at whitespace by cutPiece and rolls over into a
	// new message when it reaches the cap. Slack's own refusal of a streamed
	// message lies near 13 800 characters of text (measured on graveler
	// 2026-09-28), so the cap leaves a margin under it.
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
	// errCodeMsgTooLong is how a stream refuses content its message has no
	// room for.
	errCodeMsgTooLong = "msg_too_long"

	// chunkTypeMarkdownText is the chunk type of a streamed message's prose:
	// the answer and the agent's narration.
	chunkTypeMarkdownText = "markdown_text"
)

// batchedWriter accumulates OutboundDelta content and streams it into one
// Slack message: chat.startStream opens the reply, chat.appendStream adds
// everything accumulated since the last tick, and chat.stopStream closes it
// with the agent session's exit status. Each append carries only what is new,
// Slack animates the message while the stream is open, and the Stop button
// ends the stream on Slack's side.
//
// The whole turn is one message: the agent's interim narration and its
// answer, as markdown_text chunks in the order the agent produced them. The
// tool calls are not on it; they go to the tool log the "Inspect agent steps"
// shortcut shows. The stream opens at the first narration passage or answer
// text, whichever comes first.
//
// When the stream ends on a DeltaPrompt, run() captures it in promptDelta
// (flushing the queue first) and returns nil. The caller is responsible for
// posting the approval prompt and registering the pending task.
type batchedWriter struct {
	client   *slackAPIClient
	channel  string
	threadTS string // thread root — the reply and the turn's prompts hang off it
	logger   *slog.Logger

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
	// sessionInitiator is the Slack user ID the agent session belongs to: the
	// thread's owner, not whoever is speaking this turn. Slack applies it when
	// the status call creates the session, so it rides on the same call as the
	// title.
	sessionInitiator string
	// statusOnly marks a writer borrowed to set the session status from
	// outside a turn (Adapter.setSessionStatus), which writes nothing. Its one
	// call is the call that can create the session, so it carries the
	// initiator although it is not a processing call.
	statusOnly bool
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
	// narrationsRendered counts narration chunks queued this turn, capped so a
	// long tool-calling loop does not bury the answer. Only touched from run()'s
	// goroutine.
	narrationsRendered int
	// toolLogTurn is this turn's ordinal in the adapter's per-thread tool log
	// (see inspect.go), opened lazily on the first recorded entry; 0 until
	// then. Only touched from run()'s goroutine.
	toolLogTurn int

	mu            sync.Mutex
	queue         []queuedChunk // content accepted but not yet sent, in delta order
	flushFailures int           // consecutive failed ticker flushes; reset on success
	flushRetryAt  time.Time     // ticker flushes wait until here once they keep failing
	// appendedLen counts the answer bytes this writer has handed to streams,
	// summed across roll-overs. It is what a process continuing the turn after
	// a restart must not post again, and what says the reply carries agent text.
	appendedLen int
	promptDelta *channels.OutboundDelta // set when stream ends on DeltaPrompt
	// approvedCalls are the calls of the approval this turn resumes. The
	// resumed task streams their results and not the calls, so run() records
	// each call in the tool log before the first event, and with it the
	// call_tool target its result is named by.
	approvedCalls []channels.HitlTool
	// Stream state, touched from run()'s goroutine (and from the terminal flush
	// the adapter runs once run() has returned). streamTS is the open streamed
	// message, "" when none is open; streamed is the text it carries against
	// the size budget, so the reply rolls over into a fresh message before
	// Slack's per-message cap; streamMessages lists every streamed message of
	// the turn, which is what a retract deletes.
	streamTS       string
	streamed       int
	streamMessages []string
	// sizeBudget, once Slack refused a message the budget said had room, is
	// what that message held: a size known to fit, which bounds every later
	// message of the turn. 0 until then (see budget).
	sizeBudget int
	// streamAdopted marks streamTS as a stream a previous process opened
	// (continueFrom), which this one has not written to yet: its first text
	// goes out as an append, so a stream Slack has closed since the restart
	// recovers onto a message of its own instead of failing the closing stop.
	// streamOpened marks a message THIS writer opened, which is what says the
	// reply exists; an adopted one does
	// not count (under mu).
	streamAdopted bool
	streamOpened  bool
	// streamRecovered marks a stream Slack closed under us as already reopened
	// once this turn; streamStopped closes the text path quietly for the rest
	// of the turn (the user pressed Stop); streamFailed closes it as a failure
	// (Slack kept closing the stream), so the terminal flush reports the reply
	// as incomplete instead of ending the turn as if it were whole.
	streamRecovered bool
	streamStopped   bool
	streamFailed    bool
	// failedCalls counts the tool calls whose result was an error since this
	// reply began, so its last message can say so (failedCallsBlocks). Only
	// touched from run()'s goroutine and the terminal flush after it.
	failedCalls int

	// Continuation across a restart (continuation.go). carried is what the
	// previous process delivered of the turn this writer continues; skipText
	// is how much answer text is still to be dropped, trimLead whether the
	// whitespace the cut left in front is still to go, and leadTrimmed (under
	// mu) how much of it went. onDelivered, when set, receives the delivery
	// record after every flush.
	carried     store.Delivered
	skipText    int
	trimLead    bool
	leadTrimmed int
	onDelivered func(ctx context.Context, d store.Delivered)
	// ran is set by run(): a second cycle over the same writer opens a stream
	// of its own instead of reopening the one the previous cycle closed.
	ran bool
}

// queuedChunk is one piece of the reply's prose waiting to go out, in the order
// the agent's deltas produced it: the answer (answer true) or the agent's
// interim narration. Only a trailing answer chunk still grows, which is why the
// whitespace hold-back applies to it alone.
type queuedChunk struct {
	text   string
	answer bool
}

// textChunk renders a piece of prose as a streamed chunk.
func textChunk(md string) map[string]any {
	return map[string]any{"type": chunkTypeMarkdownText, "text": md}
}

func newBatchedWriterWithClient(client *slackAPIClient, channel, threadTS string, logger *slog.Logger) *batchedWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &batchedWriter{
		client:   client,
		channel:  channel,
		threadTS: threadTS,
		logger:   logger,
	}
}

// run drains deltas from ch, queueing the turn's text and narration in the
// order they arrive and appending what has accumulated to the turn's
// streamed message at streamAppendInterval.
func (w *batchedWriter) run(ctx context.Context, ch <-chan channels.OutboundDelta) error {
	w.timer = channels.TurnTimerFromContext(ctx)
	// A run() cycle over a writer that ran before (an auto-approved prompt
	// resuming the turn in place) continues the same turn: its answer stream
	// was closed by that cycle's final stop, so the continuation opens a message
	// of its own rather than reopening a closed one.
	if w.ran {
		w.resetStream()
	}
	w.ran = true
	for _, c := range w.approvedCalls {
		w.renderToolActivity(&channels.ToolActivity{Name: c.Name, Kind: channels.ToolCall, CallID: c.CallID, Args: c.Args})
	}
	w.approvedCalls = nil
	ticker := time.NewTicker(streamAppendInterval)
	defer ticker.Stop()
	// The session leaves "processing" on EVERY exit — stream done, stream error,
	// /stop, and the HITL prompt pause. Slack's agent loading UX does not clear
	// itself when the app posts any more, so a missing exit status leaves the
	// thread spinning for up to an hour. chat.stopStream carries a session
	// status of its own, but observed on graveler 2026-09-21 it does not clear
	// the indicator, so this call is the one that ends the session and it runs
	// on every turn. Registered before the drain so it lands after the turn's
	// last in-thread post, and before endStream so it follows the stop.
	w.setSessionStatus(ctx, sessionProcessing)
	defer func() { w.setSessionStatus(ctx, w.exitSessionStatus()) }()
	defer w.endStream(ctx) // backstop for the ctx.Done() exit; finalFlush closes first

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
				w.queueAnswer(content)
				w.openEarly(ctx)
			case channels.DeltaToolActivity:
				if d.Tool != nil && d.Tool.Kind == channels.ToolCall {
					w.timer.AddToolCall()
				}
				w.renderToolActivity(d.Tool)
				w.maybeConnectorPrompt(d.Tool)
			case channels.DeltaNarration:
				w.timer.AddChars(len(d.Content))
				w.renderNarration(d.Content)
				w.openEarly(ctx)
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

// openEarly opens the turn's stream on the first prose the agent produces — a
// narration passage or the first answer text, whichever comes first — so the
// reply message exists while the agent is still working. Everything after that
// rides the tick.
func (w *batchedWriter) openEarly(ctx context.Context) {
	if w.streamTS != "" || w.streamStopped {
		return
	}
	w.tickFlush(ctx, time.Now())
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

// Caps, in runes, of what the tool log keeps of a call's arguments (as indented
// JSON) and of its result preview.
const (
	toolArgsMax   = 800
	toolResultMax = 800
	// maxActivityBlocks bounds the blocks of one ephemeral inspection message,
	// comfortably under Slack's 50-blocks-per-message limit; an inspection
	// posted in the thread rolls over into a further message past it.
	maxActivityBlocks = 24
	// maxNarrationMessages bounds narration chunks per turn, so a long
	// tool-calling loop does not bury the answer under interim prose. Past it
	// one truncation note is sent: dropping the agent's prose without saying so
	// is the bug this rendering fixes.
	maxNarrationMessages = 10
)

// passageBreak ends a narration chunk, so two passages — or a passage and the
// answer that follows it — never run together in the message body.
const passageBreak = "\n\n"

// narrationLimitNote replaces the narration past the per-turn cap.
const narrationLimitNote = "Narration limit reached: the rest of this turn's step-by-step notes are hidden. The answer still follows."

// renderToolActivity records a tool call, or its result, in the adapter's
// per-thread tool log, which the "Inspect agent steps" shortcut shows. Nothing
// of it goes on the reply. A result that only asks for the person's approval is
// not the tool's output: it marks the call as stopped at the approval.
func (w *batchedWriter) renderToolActivity(tool *channels.ToolActivity) {
	if tool == nil {
		return
	}
	switch tool.Kind {
	case channels.ToolCall:
		name, viaMuster := tool.Name, false
		args := tool.Args
		if inner, innerArgs, ok := unwrapCallTool(tool); ok {
			// Record the call→inner mapping so the result (which carries no
			// Args) can resolve the inner name via effectiveToolName,
			// independent of whether connector prompts are enabled.
			w.noteCallToolTarget(tool)
			name, viaMuster, args = inner, true, innerArgs
		}
		w.recordToolCall(toolLogEntry{
			callID: tool.CallID, name: name, viaMuster: viaMuster,
			called: true, args: indentJSON(args, toolArgsMax),
		})
	case channels.ToolResult:
		name := w.effectiveToolName(tool)
		e := toolLogEntry{
			callID: tool.CallID, name: name,
			viaMuster: tool.Name == musterCallToolMetaTool && name != tool.Name,
			state:     toolAwaitingApproval,
		}
		if !tool.AwaitsApproval {
			var isErr bool
			e.result, isErr = toolResultPreview(tool.Response, toolResultMax)
			e.state = toolDone
			if isErr {
				e.state = toolFailed
				w.failedCalls++
			}
		}
		w.recordToolResult(e)
	}
}

// recordToolCall retains a call in the adapter's per-thread tool log, opening
// the turn's log slot on first use. w.threadTS is the thread root, the same key
// the shortcut resolves. Only called from run()'s goroutine, so toolLogTurn
// needs no lock; a resumed run() segment over the same writer (an auto-approved
// prompt) keeps recording into the same turn. Nil adapter means a direct-writer
// test; nothing to record into.
func (w *batchedWriter) recordToolCall(e toolLogEntry) {
	if w.adapter == nil || w.threadTS == "" {
		return
	}
	e.turn = w.logTurn()
	w.adapter.appendToolLog(w.threadTS, e)
}

// recordToolResult retains a result: it completes its call's entry in this
// turn, or stands on its own when no running call of this turn matches it.
func (w *batchedWriter) recordToolResult(e toolLogEntry) {
	if w.adapter == nil || w.threadTS == "" {
		return
	}
	e.turn = w.logTurn()
	w.adapter.completeToolLog(w.threadTS, e)
}

// logTurn is this turn's ordinal in the tool log, opened on first use.
func (w *batchedWriter) logTurn() int {
	if w.toolLogTurn == 0 {
		w.toolLogTurn = w.adapter.beginToolLogTurn(w.threadTS)
	}
	return w.toolLogTurn
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
	// The title and the initiator ride on the call that can CREATE the session
	// and nowhere else, because Slack ignores both on a session that exists:
	// a turn's processing call, whose exit call always follows it, or the one
	// call of a writer borrowed from outside a turn.
	title, initiator := "", ""
	if status == sessionProcessing || w.statusOnly {
		title, initiator = w.sessionTitle, w.sessionInitiator
	}
	base := context.WithoutCancel(ctx)
	var err error
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(base, sessionStatusTimeout)
		err = w.client.setSessionStatus(cctx, w.channel, w.threadTS, status, title, initiator)
		cancel()
		if status == sessionProcessing || attempt >= sessionStatusIdleAttempts || !errors.Is(err, errSessionStatusTransient) {
			break
		}
		time.Sleep(sessionStatusRetryBackoff * time.Duration(attempt))
	}
	// Slack gave a verdict against the decorated call. The title and the
	// initiator are decoration; the status is what keeps the indicator honest,
	// so send it once more bare rather than lose this turn's indicator to a
	// field Slack will not take. Only a bare call that goes through proves the
	// decoration was the cause, so only then is it named; a bare call that
	// fails too is reported once by the generic line below.
	if err != nil && (title != "" || initiator != "") && !errors.Is(err, errSessionStatusTransient) && !errors.Is(err, errSessionStatusUnsupported) {
		cctx, cancel := context.WithTimeout(base, sessionStatusTimeout)
		bare := w.client.setSessionStatus(cctx, w.channel, w.threadTS, status, "", "")
		cancel()
		if bare == nil {
			w.logger.Warn("slack: agent session title or initiator rejected, setting the status bare",
				"title_runes", utf8.RuneCountInString(title), "initiator", initiator, "error", err)
		}
		err = bare
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

// renderNarration queues the agent's interim narration — the prose it writes
// just before firing tool calls — as text chunks of the reply, so it reads in
// order with the answer that follows.
//
// Narration bytes count toward the streamed message's character cap, like any
// other prose, but NOT toward the answer length the delivery record carries:
// that number is the answer text a process continuing the turn after a restart
// must not post again, and narration is not replayed.
//
// Narration is unbounded agent prose, so it is split like the main reply: one
// chunk may not exceed slackMarkdownBlockMax. Chunks share the per-turn budget,
// so an outsized narration ends in the same limit note. A passage ends in a
// paragraph break — the chunks of one message run into one body — so what
// follows it starts a line of its own; the split pieces of a single passage get
// none, since the cut falls mid-prose.
func (w *batchedWriter) renderNarration(text string) {
	scrubbed := strings.TrimSpace(w.scrubLoginURLs(text))
	if scrubbed == "" {
		return
	}
	pieces := splitMarkdown(scrubbed, slackMarkdownBlockMax)
	for i, md := range pieces {
		w.narrationsRendered++
		switch {
		case w.narrationsRendered > maxNarrationMessages+1:
			return // already queued the truncation note
		case w.narrationsRendered == maxNarrationMessages+1:
			w.queueNarration(narrationLimitNote + passageBreak)
			return
		}
		if i == len(pieces)-1 {
			md += passageBreak
		}
		w.queueNarration(md)
	}
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

// indentJSON renders a call's arguments as indented JSON, one key per line,
// truncated to max runes; "" for a call without arguments.
func indentJSON(v map[string]any, max int) string {
	if len(v) == 0 {
		return ""
	}
	return indentJSONValue(v, max)
}

// indentJSONValue is indentJSON over any JSON value, so a result that is an
// array renders like one that is an object; "" for a value that does not
// marshal.
func indentJSONValue(v any, max int) string {
	// json.Marshal is HTML-safe: it spells <, > and & as \u003c, \u003e and
	// \u0026. That neutralising is the wrong layer here — the inspection
	// escapes every payload for mrkdwn itself (escapeMrkdwn) — and it put the
	// agent's own PromQL on screen as "\u003e 0.5" (graveler, 2026-09-22).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return truncateRunes(strings.TrimRight(buf.String(), "\n"), max)
}

// maxMCPResultUnwrapDepth bounds the unwrapping of nested serialized MCP
// results. muster's call_tool serializes the inner tool's whole result as the
// outer envelope's text, so real payloads arrive double-wrapped (and the inner
// text is often itself a JSON document); anything deeper is hostile or broken
// input, rendered as-is.
const maxMCPResultUnwrapDepth = 4

// toolResultPreview renders a tool result payload for the tool log, spending
// the max budget on the innermost actual payload instead of envelope
// boilerplate. An MCP result envelope ({"content": [...], "isError": ...}) or
// kagent's plain-output wrap ({"output": text}) is reduced to its text; text
// that is itself a serialized JSON document (muster's call_tool re-wrap) is
// decoded and unwrapped again up to maxMCPResultUnwrapDepth. The innermost
// payload renders as indented JSON when it is a JSON document, like the call's
// arguments, and as whitespace-collapsed plain text otherwise. isErr reports
// whether any unwrapped envelope flagged the result as an error. Payloads with
// any other shape render unchanged as indented JSON.
func toolResultPreview(resp map[string]any, max int) (preview string, isErr bool) {
	text, isErr, ok := toolResultText(resp)
	if !ok {
		// Not a text carrier, but the error flag is honoured wherever the
		// payload carries it.
		return indentJSON(resp, max), isErr
	}
	for depth := 0; depth < maxMCPResultUnwrapDepth; depth++ {
		v, isJSON := decodeJSONDocument(text)
		if !isJSON {
			break
		}
		if m, isMap := v.(map[string]any); isMap {
			inner, innerErr, isEnvelope := toolResultText(m)
			isErr = isErr || innerErr
			if isEnvelope {
				text = inner
				continue
			}
		}
		return indentJSONValue(v, max), isErr
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		// An envelope with no text content (empty content list, non-text items
		// only): fall back to the raw payload so the entry still shows something.
		return indentJSON(resp, max), isErr
	}
	return truncateRunes(text, max), isErr
}

// toolResultText extracts the text a tool result payload carries. ok reports
// whether the payload is a recognized text carrier: an MCP tool-result
// envelope ({"content": [{"type": "text", "text": ...}, ...], "isError": ...}),
// whose text items are joined and whose non-text items render as a [type]
// placeholder, or the ADK/kagent single-key wrap around a plain tool output
// ({"output": text} or {"result": text}, depending on the tool type), or the
// ADK runtime's single-key wrap around a failed call ({"error": text}): adk-go
// turns every tool error, an MCP isError result included, into that shape. Any
// other shape yields ok false so the caller keeps the raw JSON rendering.
//
// isErr is read wherever the payload carries it, not only inside an envelope:
// kagent's harness runtime sets it beside the wrap ({"result": …, "isError":
// true}), so that one key, as a boolean, is allowed next to a carrier, and the
// flag is reported even when the shape is otherwise not recognised.
func toolResultText(v map[string]any) (text string, isErr, ok bool) {
	isErr, _ = v["isError"].(bool)
	items, isEnvelope := v["content"].([]any)
	if !isEnvelope {
		carrierKeys := len(v)
		if _, flagged := v["isError"].(bool); flagged {
			carrierKeys--
		}
		if carrierKeys == 1 {
			for _, key := range []string{"output", "result"} {
				if out, isText := v[key].(string); isText {
					return out, isErr, true
				}
			}
			if msg, isText := v["error"].(string); isText {
				return msg, true, true
			}
		}
		return "", isErr, false
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		m, isMap := item.(map[string]any)
		if !isMap {
			return "", isErr, false
		}
		if s, hasText := m["text"].(string); hasText {
			parts = append(parts, s)
			continue
		}
		typ, hasType := m["type"].(string)
		if !hasType {
			return "", isErr, false
		}
		parts = append(parts, "["+typ+"]")
	}
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

// wroteContent reports whether this writer left a message of its own in the
// thread. Used after the run loop to pick a terminal note: an empty-output
// note only when no reply exists, a failure note that says the reply is
// incomplete once one does. The reply need not carry answer text — narration
// lives in it too, and a turn that produced only narration has a message all
// the same. A stream merely adopted from a previous process is not one: that
// message is already on screen, and a continued turn with nothing to add still
// has its say.
func (w *batchedWriter) wroteContent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.appendedLen > 0 || w.streamOpened
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

// retractRendered deletes the turn's streamed messages, leaving the thread to
// the connector prompt alone: the reply is where the sign-in narration the
// button contradicts lives. The tool log the "Inspect
// agent steps" shortcut shows is untouched — it is a transparency record, not
// prose on screen. Best-effort: a delete failure is logged, not propagated.
func (w *batchedWriter) retractRendered(ctx context.Context) {
	// A message still streaming cannot be retracted, so close it first. The
	// turn is being taken over by the sign-in prompt and is not over, hence
	// processing rather than Slack's active default. Beware the ordering: this
	// runs after run() has set the session's exit status, so the day Slack
	// honours session_status on a stop, this one would re-arm an indicator the
	// turn just cleared. It is unreachable today — closeStream clears streamTS
	// on a successful stop and on a stream-gone refusal, and the retract is
	// only reached on a turn that ended normally.
	if w.streamTS != "" {
		if err := w.stopStream(ctx, nil, nil, 0, sessionProcessing); err != nil {
			w.logger.Warn("slack: stop the reply stream before retracting it failed", "error", err)
		}
	}
	w.dropStream()
	w.mu.Lock()
	messages := w.streamMessages
	w.streamMessages, w.streamOpened, w.appendedLen = nil, false, 0
	w.mu.Unlock()
	// The row must stop naming text and a message the thread no longer has.
	w.noteDelivered(ctx)
	for _, ts := range messages {
		if err := w.client.deleteMessage(ctx, w.channel, ts); err != nil {
			w.logger.Warn("slack: retract connector reply failed", "ts", ts, "error", err)
		}
	}
}

// flush sends everything queued since the last one to the turn's stream,
// opening the stream on the first call. The answer text after the last
// whitespace is held back: an append is final — unlike the chat.update it
// replaces, which re-rendered the whole reply every tick — so a login URL must
// never be sent half-scrubbed and a word never cut in two.
func (w *batchedWriter) flush(ctx context.Context) error {
	if w.streamStopped {
		w.takeQueued(true) // the reply is closed; drop what is left
		return nil
	}
	if w.streamFailed {
		w.takeQueued(true)
		return errStreamLost
	}
	items := w.takeQueued(false)
	if len(items) == 0 {
		return nil
	}
	if unsent, err := w.sendQueued(ctx, items, false); err != nil {
		w.putBackQueued(unsent)
		return err
	}
	return nil
}

// closeStream ends the turn's reply: everything still queued rides the
// chat.stopStream that closes the streamed message, together with the session's
// exit status. Content the open message has no room for goes out as appends
// first, and a turn whose whole reply is still queued opens its stream here.
// With no stream and nothing queued it does nothing, leaving the exit status to
// run()'s own call.
func (w *batchedWriter) closeStream(ctx context.Context) error {
	if w.streamStopped {
		w.takeQueued(true)
		return nil
	}
	if w.streamFailed {
		w.takeQueued(true)
		return errStreamLost
	}
	items := w.takeQueued(true)
	if len(items) == 0 && w.streamTS == "" {
		return nil
	}
	if unsent, err := w.sendQueued(ctx, items, true); err != nil {
		w.putBackQueued(unsent)
		return err
	}
	return nil
}

// endStream closes a stream a turn left open. A cancelled turn (a /stop, the
// gateway shutting down) returns without a terminal flush, so without this the
// message would keep animating and everything queued since the last append
// would be lost. It runs on a context outliving the cancellation.
func (w *batchedWriter) endStream(ctx context.Context) {
	if w.streamTS == "" && !w.hasQueuedContent() {
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

// streamCallCtx detaches one write to the streamed message from the turn's
// cancellation and bounds it. A write Slack may already have taken has to run
// to a real answer: a cancelled call comes back as an error the writer cannot
// tell from a refusal, and re-queueing the text then posts it twice — which is
// what pressing Stop mid-chunk used to do. An enclosing deadline that is nearer
// still wins, so the closing pass keeps its own budget.
func streamCallCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := streamCallTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < timeout {
			timeout = left
		}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// streamBatch accumulates the chunks destined for one streamed message: the
// pieces taken off the queue since the last Slack call, and the byte counts
// they stand for. The pieces are kept as they were queued, so a delivery that
// fails hands back exactly what did not land.
type streamBatch struct {
	items []queuedChunk
	// answerRaw is the agent's own answer bytes the batch carries, what the
	// delivery record counts — the text a process continuing the turn must not
	// post again. cost is what the batch adds to the message's size budget:
	// its prose and narration bytes.
	answerRaw int
	cost      int
}

func (b *streamBatch) addText(piece string, answer bool) {
	b.items = append(b.items, queuedChunk{text: piece, answer: answer})
	b.cost += len(piece)
	if answer {
		b.answerRaw += len(piece)
	}
}

func (b *streamBatch) empty() bool { return len(b.items) == 0 }

func (b *streamBatch) reset() { *b = streamBatch{} }

// budget is the size one streamed message of this turn may reach.
func (w *batchedWriter) budget() int {
	if w.sizeBudget > 0 {
		return w.sizeBudget
	}
	return slackMarkdownBlockMax
}

// room is what the open message's size budget has left once the batch lands.
func (w *batchedWriter) room(b *streamBatch) int {
	return w.budget() - w.streamed - b.cost
}

// makeRoom lands what the batch holds or, with nothing held, rolls the open
// message over, so the next pass has room.
func (w *batchedWriter) makeRoom(ctx context.Context, b *streamBatch) error {
	if b.empty() {
		return w.rollOverStream(ctx)
	}
	return w.deliverBatch(ctx, b)
}

// sendQueued delivers queued content on the turn's stream and returns what it
// could not deliver, so the caller re-queues exactly what is missing. Prose is
// measured on the agent's own bytes: that is the unit a process continuing the
// turn after a restart skips, so it has to be the unit the pieces are cut in as
// well. When closing, the last batch rides the chat.stopStream that ends the
// reply instead of an append of its own. What a message Slack refused to grow
// could not take goes on to a new message in the same call (overflowStream):
// that is a delivery still under way, not a failed one to retry later.
func (w *batchedWriter) sendQueued(ctx context.Context, items []queuedChunk, closing bool) (unsent []queuedChunk, err error) {
	for {
		unsent, err = w.sendQueuedOnce(ctx, items, closing)
		if !errors.Is(err, errStreamOverflow) {
			return unsent, err
		}
		if w.streamStopped {
			// The stop closing the full message found the user had pressed
			// Stop: the rest of the reply is dropped, like any after a Stop.
			return nil, nil
		}
		items = unsent
	}
}

// sendQueuedOnce is one pass of sendQueued, which ends early when a message
// overflows.
func (w *batchedWriter) sendQueuedOnce(ctx context.Context, items []queuedChunk, closing bool) (unsent []queuedChunk, err error) {
	var batch streamBatch
	// left is everything Slack has not taken: what the batch still holds, the
	// untaken tail of the piece being cut, and the items after it.
	left := func(i int, raw string, answer bool) []queuedChunk {
		rest := slices.Clone(batch.items)
		if raw != "" {
			rest = append(rest, queuedChunk{text: raw, answer: answer})
		}
		return append(rest, items[i+1:]...)
	}
	for i := range items {
		raw, answer := items[i].text, items[i].answer
		for raw != "" {
			piece, rest := cutPiece(raw, w.budget())
			// streamed counts the agent's bytes, not the shorter text Slack
			// sees: scrubbing only ever removes, so the count over-estimates and
			// rolls the reply over a little early — the safe side of Slack's
			// per-message cap. Should Slack refuse it all the same,
			// overflowStream moves it on.
			if len(piece) > w.room(&batch) {
				// No room for this piece. Land what the batch already holds
				// first — that may be all it takes, since the delivery can end
				// up on a message of its own — and only then roll the open one
				// over. Either way the next pass has room, so the loop advances.
				if err := w.makeRoom(ctx, &batch); err != nil {
					return left(i, raw, answer), err
				}
				if w.streamStopped {
					return nil, nil
				}
				continue
			}
			batch.addText(piece, answer)
			raw = rest
		}
	}
	if closing {
		return w.closeBatch(ctx, &batch)
	}
	if err := w.deliverBatch(ctx, &batch); err != nil {
		return batch.items, err
	}
	return nil, nil
}

// chunkBodies renders the batch's pieces as streamed chunks. This turn's login
// URLs are scrubbed out of every piece of prose here, at the last moment, so a
// link the agent surfaced after a passage was queued is caught too; a piece
// scrubbing empties carries nothing and is left out. visible reports whether
// the batch holds anything a message can be opened on — prose that is
// whitespace alone never is.
func (w *batchedWriter) chunkBodies(items []queuedChunk) (chunks []any, visible bool) {
	for _, it := range items {
		md := w.scrubLoginURLs(it.text)
		if md == "" {
			continue
		}
		chunks = append(chunks, textChunk(md))
		visible = visible || strings.TrimSpace(md) != ""
	}
	return chunks, visible
}

// deliverBatch hands the batch to Slack — chat.startStream when no message is
// open, chat.appendStream otherwise — and empties it once it has landed.
func (w *batchedWriter) deliverBatch(ctx context.Context, b *streamBatch) error {
	if b.empty() {
		return nil
	}
	chunks, visible := w.chunkBodies(b.items)
	// Scrubbing a pure sign-in passage can empty a piece, and a stream does not
	// open on whitespace alone. Nothing reaches Slack, but the bytes count as
	// delivered all the same: a process continuing the turn would only scrub
	// them away again.
	if len(chunks) == 0 || (w.streamTS == "" && !visible) {
		w.noteAppended(b.answerRaw)
		w.noteDelivered(ctx)
		b.reset()
		return nil
	}
	if w.streamTS == "" {
		return w.openStream(ctx, b, chunks)
	}
	sctx, cancel := streamCallCtx(ctx)
	err := w.client.appendStream(sctx, w.channel, w.streamTS, chunks)
	cancel()
	switch {
	case err == nil:
		w.streamAdopted = false
		w.landBatch(b)
		w.noteAppended(b.answerRaw)
		w.noteDelivered(ctx)
		b.reset()
		return nil
	case errors.Is(err, errStreamStoppedByUser):
		w.endStreamQuietly()
		b.reset()
		return nil
	case streamGone(err):
		return w.recoverStream(ctx, b, chunks)
	case hasErrorCode(err, errCodeMsgTooLong) && w.heldContent():
		return w.overflowStream(ctx, b, err)
	}
	return err
}

// landBatch books a batch the open message took against its size budget.
func (w *batchedWriter) landBatch(b *streamBatch) {
	w.streamed += b.cost
}

// errStreamOverflow reports that Slack refused a batch its message had no
// room for, although the size budget said it fit. The message is closed by
// then, and what the batch still holds goes out on a new one.
var errStreamOverflow = errors.New("slack: the reply outgrew its message")

// heldContent reports whether the open message took content before: this
// writer's own, or an adopted stream's from before a restart, whose record may
// count none.
func (w *batchedWriter) heldContent() bool {
	return w.streamed > 0 || w.streamAdopted
}

// overflowStream moves the rest of the reply off a message Slack refused to
// grow: the message is closed and the batch keeps what it holds, for
// sendQueued to deliver on a new message. What the full message held becomes
// the turn's budget, so the later messages stay at a size Slack took. It runs
// only when the message held content (see heldContent), so a batch too long
// for any message is not moved on and on.
func (w *batchedWriter) overflowStream(ctx context.Context, b *streamBatch, cause error) error {
	w.sizeBudget = max(min(w.streamed, w.budget()), slackMarkdownBlockMax/4)
	if err := w.rollOverStream(ctx); err != nil {
		return err
	}
	w.logger.Warn("slack: the reply outgrew its message, continuing in a new one",
		"channel", w.channel, "thread", w.threadTS, "error", cause)
	return fmt.Errorf("%w: %w", errStreamOverflow, cause)
}

// rollOverStream closes the message the reply outgrew, so what follows opens
// one of its own. The turn is still running, so the stop leaves the session
// processing; Slack's default (active) would clear the working indicator
// mid-answer.
func (w *batchedWriter) rollOverStream(ctx context.Context) error {
	if w.streamTS == "" {
		return nil
	}
	if err := w.stopStream(ctx, nil, nil, 0, sessionProcessing); err != nil {
		if !streamGone(err) {
			return err
		}
		w.dropStream() // already closed; the rest opens a message of its own
	}
	return nil
}

// closeBatch lands the reply's last chunks and closes the streamed message with
// the session's exit status, in one call where it can. A stream adopted from
// before a restart takes the append path first: Slack may have closed it since,
// and only that path can move the content onto a stream of its own.
func (w *batchedWriter) closeBatch(ctx context.Context, b *streamBatch) (unsent []queuedChunk, err error) {
	if w.streamTS == "" || w.streamAdopted {
		if err := w.deliverBatch(ctx, b); err != nil {
			return b.items, err
		}
		if w.streamTS == "" { // stopped under us, or nothing worth a message
			return nil, nil
		}
	}
	// The exit status rides the stop as the field Slack documents, and run()'s
	// own agents.sessions.setStatus sends it again on the way out: observed on
	// graveler 2026-09-21 that Slack accepts session_status here but the
	// working indicator does not clear, so that call is the source of truth.
	chunks, _ := w.chunkBodies(b.items)
	err = w.stopStream(ctx, chunks, w.failedCallsBlocks(), b.answerRaw, w.exitSessionStatus())
	switch {
	case err == nil:
		b.reset()
		return nil, nil
	case streamGone(err):
		// Nothing is left to close: a stream adopted from before a restart that
		// Slack has closed since, most often. What the stop was carrying goes
		// back to the queue so a retry can give it a stream of its own.
		w.dropStream()
		w.noteDelivered(ctx)
		if b.empty() {
			return nil, nil
		}
	case hasErrorCode(err, errCodeMsgTooLong) && w.heldContent():
		// sendQueued opens a message for what is left and closes the reply there.
		err = w.overflowStream(ctx, b, err)
	}
	return b.items, err
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

// openStream posts the turn's streamed message with its first chunks. In a
// channel Slack requires the recipient the answer is for.
func (w *batchedWriter) openStream(ctx context.Context, b *streamBatch, chunks []any) error {
	user, team := w.streamRecipient()
	sctx, cancel := streamCallCtx(ctx)
	ts, err := w.client.startStream(sctx, w.channel, w.threadTS, chunks, user, team)
	cancel()
	if err != nil {
		return err
	}
	w.streamTS, w.streamAdopted = ts, false
	w.landBatch(b)
	w.mu.Lock()
	w.streamMessages = append(w.streamMessages, ts)
	w.streamOpened = true
	w.mu.Unlock()
	w.noteStream(streamEventStarted)
	w.noteAppended(b.answerRaw)
	w.noteDelivered(ctx)
	b.reset()
	return nil
}

// stopStream closes the open stream with a last batch of chunks and the session
// status the stop leaves the thread in. rawLen is the answer bytes the chunks
// stand for — it differs from what Slack receives when a login URL was scrubbed
// out, and the delivery record counts the answer's own bytes. The status is
// always explicit: Slack defaults it to active, which on an intermediate stop
// would clear the working indicator while the turn keeps running.
func (w *batchedWriter) stopStream(ctx context.Context, chunks, blocks []any, rawLen int, status sessionStatus) error {
	sctx, cancel := streamCallCtx(ctx)
	err := w.client.stopStream(sctx, w.channel, w.streamTS, chunks, blocks, status)
	cancel()
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
// streaming, and delivers the batch on the new one. One recovery per turn: a
// second says the stream cannot be kept open, and scattering the rest of the
// answer over fresh messages would read worse than stopping there — so the turn
// gives up on the reply and says so in the thread.
func (w *batchedWriter) recoverStream(ctx context.Context, b *streamBatch, chunks []any) error {
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
	return w.openStream(ctx, b, chunks)
}

// endStreamQuietly closes the text path for the rest of the turn after Slack
// answered a stream call with stopped_by_user: Slack has ended the stream, so
// nothing more is sent on it — no retry, no error notice. The stop button's own
// "Stopped by …" notice is the thread's record of what happened.
//
// This is defensive, not the mechanism: on graveler 2026-09-21 a Stop press
// never produced stopped_by_user — the writer's own calls kept succeeding — and
// the turn ended through its cancelled context instead (endStream closes the
// stream on the way out). Nothing may depend on this branch firing.
func (w *batchedWriter) endStreamQuietly() {
	w.dropStream()
	w.streamStopped = true
	w.takeQueued(true)
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
	w.streamRecovered, w.streamStopped, w.streamFailed = false, false, false
	w.failedCalls = 0
}

// failedCallsBlocks is what the reply's last message ends with when tool calls
// of this reply failed: a muted line that counts them and a button that opens
// the thread's "Agent steps" modal; nil when none did. The reply does not show
// the calls, so without it a failure is visible only when the agent says so.
func (w *batchedWriter) failedCallsBlocks() []any {
	if w.failedCalls == 0 {
		return nil
	}
	return []any{
		contextBlock(failedCallsNote(w.failedCalls)),
		map[string]any{
			bkType: bkActions,
			bkElements: []any{map[string]any{
				bkType:     bkButton,
				bkText:     plainTextObj(failedCallsButton),
				bkActionID: inspectOpenAction,
			}},
		},
	}
}

// failedCallsButton labels the button of failedCallsBlocks.
const failedCallsButton = "Show tool calls"

// failedCallsNote is the text of failedCallsBlocks for n failed calls.
func failedCallsNote(n int) string {
	if n == 1 {
		return "⚠️ 1 tool call failed"
	}
	return fmt.Sprintf("⚠️ %d tool calls failed", n)
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

// queueAnswer accepts a piece of the agent's answer, merging it into the
// trailing answer chunk when there is one: the answer arrives token by token,
// and one chunk per token would say nothing a single one does not.
func (w *batchedWriter) queueAnswer(text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := len(w.queue); n > 0 && w.queue[n-1].answer {
		w.queue[n-1].text += text
		return
	}
	w.queue = append(w.queue, queuedChunk{text: text, answer: true})
}

// queueNarration accepts one chunk of the agent's interim prose.
func (w *batchedWriter) queueNarration(md string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.queue = append(w.queue, queuedChunk{text: md})
}

// takeQueued removes the content to send from the queue. Unless final, a
// trailing answer chunk keeps everything after its last whitespace: it may be
// half a word or half a login URL, and an appended chunk cannot be taken back.
// Narration is complete when it is queued, so nothing is ever held back for
// it.
func (w *batchedWriter) takeQueued(final bool) []queuedChunk {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(w.queue)
	if n == 0 {
		return nil
	}
	last := w.queue[n-1]
	if final || !last.answer {
		taken := w.queue
		w.queue = nil
		return taken
	}
	i := strings.LastIndexFunc(last.text, unicode.IsSpace)
	if i < 0 {
		// The growing chunk holds no whitespace at all: hold it back whole.
		taken := slices.Clone(w.queue[:n-1])
		w.queue = []queuedChunk{last}
		return taken
	}
	_, size := utf8.DecodeRuneInString(last.text[i:])
	taken := append(slices.Clone(w.queue[:n-1]), queuedChunk{text: last.text[:i+size], answer: true})
	if rest := last.text[i+size:]; rest != "" {
		w.queue = []queuedChunk{{text: rest, answer: true}}
	} else {
		w.queue = nil
	}
	return taken
}

// putBackQueued re-queues what a delivery did not land, ahead of whatever
// arrived meanwhile, so the next flush re-sends it in order.
func (w *batchedWriter) putBackQueued(items []queuedChunk) {
	if len(items) == 0 {
		return
	}
	w.mu.Lock()
	w.queue = append(slices.Clone(items), w.queue...)
	w.mu.Unlock()
}

// hasQueuedContent reports whether anything queued would reach Slack: prose
// that is more than whitespace.
func (w *batchedWriter) hasQueuedContent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, it := range w.queue {
		if strings.TrimSpace(it.text) != "" {
			return true
		}
	}
	return false
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

// RateLimitRecorder counts the Web API calls Slack answers with a 429; the
// observability package implements it with a labelled counter. Nil is fine
// wherever a recorder is optional.
type RateLimitRecorder interface {
	RecordSlackRateLimit(method, outcome string)
}

// What call() did with a 429 (see RateLimitRecorder): it waited Retry-After
// and tried again, or it gave up — the attempt budget spent or the requested
// wait over rateLimitRetryCap — and failed the call.
const (
	rateLimitRetried   = "retried"
	rateLimitExhausted = "exhausted"
)

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
	// rateLimits, when set, counts every 429 Slack answers with. Nil in tests
	// that construct the client directly; every client the adapter builds
	// carries the adapter's recorder.
	rateLimits RateLimitRecorder
}

// noteRateLimited counts one 429 under what the client did about it. A client
// built without a recorder counts nothing.
func (c *slackAPIClient) noteRateLimited(method, outcome string) {
	if c.rateLimits == nil {
		return
	}
	c.rateLimits.RecordSlackRateLimit(method, outcome)
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

// viewsUpdate replaces an open modal's view. hash is the version the caller
// saw: Slack refuses the update (hash_conflict) when the view changed since.
func (c *slackAPIClient) viewsUpdate(ctx context.Context, viewID, hash string, view map[string]any) error {
	body := map[string]any{paramViewID: viewID, paramView: view}
	if hash != "" {
		body[paramHash] = hash
	}
	_, err := c.postJSON(ctx, "views.update", body)
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
	// replace_original false: from a button on a normal message, a response_url
	// replaces that message unless told otherwise.
	data, err := json.Marshal(map[string]any{"response_type": "ephemeral", "replace_original": false, paramText: text})
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

// lookupUserByEmail returns the Slack user ID of the workspace member with
// email; users_not_found is ErrAddresseeNotFound. Needs users:read.email.
func (c *slackAPIClient) lookupUserByEmail(ctx context.Context, email string) (string, error) {
	params := url.Values{"email": {email}}
	body, err := c.call(ctx, "users.lookupByEmail", "application/x-www-form-urlencoded", params.Encode())
	if err != nil {
		return "", err
	}
	var result struct {
		OK   bool   `json:"ok"`
		Err  string `json:"error,omitempty"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack users.lookupByEmail: decode: %w", err)
	}
	switch {
	case result.Err == "users_not_found":
		return "", channels.ErrAddresseeNotFound
	case !result.OK:
		return "", &apiError{method: "users.lookupByEmail", code: result.Err}
	}
	return result.User.ID, nil
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
// initiator is the person the session belongs to, applied on creation like the
// title. Without it Slack reads the starter off the thread root, which is the
// bot's own message whenever the picker opened the conversation. Empty is
// omitted rather than sent blank.
//
// The call goes out unbranded on purpose: the display-identity fields would
// need chat:write.customize, and its missing_scope rejection is
// indistinguishable from the one that latches this method off for the whole
// process.
func (c *slackAPIClient) setSessionStatus(ctx context.Context, channelID, threadTS string, status sessionStatus, title, initiator string) error {
	params := map[string]any{
		paramChannelID: channelID,
		paramThreadTS:  threadTS,
		paramStatus:    string(status),
	}
	if title != "" {
		params[paramTitle] = title
	}
	if initiator != "" {
		params[paramInitiatorUserID] = initiator
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

// postQuestion posts the question that opens a conversation from the agent
// picker: the question as the message, and in a channel who asked as a context
// line under it. A direct message leaves that line out — the message already
// carries the agent's name, and the only person who can read it is the one who
// asked.
func (c *slackAPIClient) postQuestion(ctx context.Context, channel, question, user, threadTS string) (string, error) {
	// Escaping can grow a question the modal capped at slackSectionTextMax.
	text := escapeMrkdwn(question)
	if r := []rune(text); len(r) > slackSectionTextMax {
		cut := string(r[:slackSectionTextMax-1])
		// An entity cut in half would show as "&am…". After escaping, every
		// "&" starts an entity that ends with ";".
		if i := strings.LastIndexByte(cut, '&'); i > strings.LastIndexByte(cut, ';') {
			cut = cut[:i]
		}
		text = cut + "…"
	}
	blocks := []any{map[string]any{
		bkType: bkSection,
		bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
	}}
	if !isDMChannelID(channel) {
		blocks = append(blocks, contextBlock(fmt.Sprintf(askAgentAskedBy, user)))
	}
	body := map[string]any{
		paramChannel: channel,
		paramText:    text,
		paramBlocks:  blocks,
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// contextBlock wraps mrkdwn in a Block Kit context block, which Slack renders
// as small muted text — visually subordinate to the agent's prose, which is
// what tool transparency and a message's metadata should be.
func contextBlock(md string) map[string]any {
	return map[string]any{
		bkType:     bkContext,
		bkElements: []any{map[string]any{bkType: bkMrkdwn, bkText: md}},
	}
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

// startStream opens a streaming message in the thread carrying its first
// chunks and returns its ts. recipientUser and recipientTeam name the person
// the answer is for: Slack requires both when the stream is in a channel and
// refuses them in a DM, so the caller passes them only for a channel. The
// display identity rides along as it does on a new post, including the
// unbranded retry when the customize scope is missing (see postJSON).
func (c *slackAPIClient) startStream(ctx context.Context, channel, threadTS string, chunks []any, recipientUser, recipientTeam string) (string, error) {
	body := map[string]any{
		paramChannel: channel,
		paramChunks:  chunks,
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

// appendStream adds chunks to an open stream. Unlike chat.update it carries
// only what is new, so a long answer costs one small call per tick.
func (c *slackAPIClient) appendStream(ctx context.Context, channel, ts string, chunks []any) error {
	_, err := c.postJSON(ctx, methodChatAppendStream, map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramChunks:  chunks,
	})
	return streamErr(err)
}

// stopStream closes a stream, optionally with a last batch of chunks, and names
// the thread's agent session status in the same call. status is always sent:
// Slack defaults the field to active, which would be wrong on a stop that only
// rolls the answer over into a new message. Observed on graveler 2026-09-21
// that Slack accepts the field but the working indicator does not clear with
// it, so the turn also ends the session through agents.sessions.setStatus.
func (c *slackAPIClient) stopStream(ctx context.Context, channel, ts string, chunks, blocks []any, status sessionStatus) error {
	body := map[string]any{
		paramChannel:       channel,
		paramTS:            ts,
		paramSessionStatus: string(status),
	}
	if len(chunks) > 0 {
		body[paramChunks] = chunks
	}
	// blocks render at the bottom of the finalized message (the method
	// reference), below everything the stream carried.
	if len(blocks) > 0 {
		body[paramBlocks] = blocks
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

// postApprovalPrompt posts the approval card of a HITL tool approval: the
// section card (approvalCard), who may decide, and the Approve and Deny
// buttons. The button values encode the thread (routing) and the task
// the prompt renders (staleness check).
func (c *slackAPIClient) postApprovalPrompt(ctx context.Context, channel, threadID, taskID, card, initiator string) (string, error) {
	blocks := []any{
		map[string]any{
			bkType: bkSection,
			bkText: map[string]any{bkType: bkMrkdwn, bkText: card},
		},
	}
	if initiator != "" {
		blocks = append(blocks, contextBlock(fmt.Sprintf(approvalDeciders, initiator)))
	}
	value := encodeHitlValue(threadID, taskID)
	blocks = append(blocks, map[string]any{
		bkType: bkActions,
		bkElements: []any{
			map[string]any{bkType: bkButton, bkText: plainTextObj(approvalApproveLabel), bkStyle: bkPrimary, bkActionID: hitlApprove, bkValue: value},
			map[string]any{bkType: bkButton, bkText: plainTextObj(approvalDenyLabel), bkStyle: bkDanger, bkActionID: hitlDeny, bkValue: value},
		},
	})
	return c.postJSON(ctx, methodChatPostMessage, map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     card,
		paramBlocks:   blocks,
	})
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
func (c *slackAPIClient) postChoiceWidgetPrompt(ctx context.Context, channel, threadID, taskID, question string, choices []string, multiple bool) (string, error) {
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
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// postChoiceFormPrompt posts a multi-question ask_user prompt as a single form:
// a question section plus a radio/checkbox widget per question, all committed by
// one Submit. Each question's widget block_id encodes its question index
// (hitlQGroupPrefix + "_<qi>") so the handler maps each selection back to its
// question. The caller (formRenderable) guarantees every question is widgetable.
func (c *slackAPIClient) postChoiceFormPrompt(ctx context.Context, channel, threadID, taskID string, questions []channels.HitlQuestion) (string, error) {
	blocks := make([]any, 0, 2*len(questions)+1)
	for qi, q := range questions {
		blocks = append(blocks, questionSection(q.Question))
		blocks = append(blocks, choiceWidgetBlock(fmt.Sprintf("%s_%d", hitlQGroupPrefix, qi), q.Choices, q.Multiple))
	}
	blocks = append(blocks, submitActions(threadID, taskID))
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     "Answer the questions below.",
		paramBlocks:   blocks,
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// postChoiceSectionPrompt posts an ask_user question whose choices are too long
// for a widget option's 75-rune text: one section block per choice carries the
// full label (up to Slack's 3000-char section limit) with a selection control.
// Single-select uses an accessory button per row (a click commits, since one
// choice per row is unambiguous); multi-select uses an accessory single-option
// checkbox per row plus a Submit button, and the handler gathers the selected
// rows out of state.values.
func (c *slackAPIClient) postChoiceSectionPrompt(ctx context.Context, channel, threadID, taskID, question string, choices []string, multiple bool) (string, error) {
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
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// signInTrigger is what asked for a sign-in prompt. It picks the card's last
// line: only a held message runs by itself once the link completes.
type signInTrigger int

const (
	signInForMessage signInTrigger = iota // an unlinked user's message, held for replay
	signInForLogin                        // the /login command: nothing is held
	signInForClick                        // a button click: nothing to replay, the person clicks again
	signInForReply                        // a decision answered in its thread: nothing held, the person replies again
)

// signInPromptBody builds the sign-in card's Slack post body: when it replaces
// a prompt whose link expired, a context line saying so; the title and body
// with the trigger's line; a "Sign in" URL button opening linkURL; and the
// context line for a person whose earlier session ended.
func signInPromptBody(channel, threadID, linkURL, promptID string, supersedes bool, trigger signInTrigger) map[string]any {
	text := "*" + signInPromptTitle + "*\n" + fmt.Sprintf(signInPromptBodyFormat, int(signInNudgeTTL/time.Minute))
	switch trigger {
	case signInForMessage:
		text += " " + signInForMessageLine
	case signInForClick:
		text += " " + signInForClickLine
	case signInForReply:
		text += " " + signInForReplyLine
	}
	signInButton := map[string]any{
		bkType:     bkButton,
		bkText:     map[string]any{bkType: bkPlainText, bkText: "Sign in"},
		bkStyle:    bkPrimary,
		bkActionID: oboSignIn,
		bkURL:      linkURL,
	}
	// A channel prompt's click names its thread and the prompt, so the handler
	// files the click's response_url under the prompt's (user, thread) anchor
	// only while that anchor is still this prompt: a click on an older card in
	// the thread must not take the handle of the current one. A DM prompt is
	// rewritten by its ts and carries no value.
	if promptID != "" {
		signInButton[bkValue] = encodeSignInValue(threadID, promptID)
	}
	var blocks []any
	if supersedes {
		blocks = append(blocks, contextBlock(signInLinkSupersededNote))
	}
	blocks = append(blocks,
		map[string]any{
			bkType: bkSection,
			bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
		},
		map[string]any{
			bkType:     bkActions,
			bkElements: []any{signInButton},
		},
		contextBlock(signInSessionHint),
	)
	fallback := text
	if supersedes {
		fallback = signInLinkSupersededNote + "\n" + text
	}
	body := map[string]any{
		paramChannel: channel,
		paramText:    fallback,
		paramBlocks:  blocks,
	}
	if threadID != "" {
		body[paramThreadTS] = threadID
	}
	return body
}

// postBlocks posts a Block Kit message with text as its notification fallback.
func (c *slackAPIClient) postBlocks(ctx context.Context, channel, threadID, text string, blocks []any) (string, error) {
	body := map[string]any{
		paramChannel: channel,
		paramText:    text,
		paramBlocks:  blocks,
	}
	if threadID != "" {
		body[paramThreadTS] = threadID
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// postNote posts one of the gateway's own notes (a stop, a failure, a refusal)
// in the metadata register: a context block, Slack's small muted text, so it
// reads apart from the agent's answer.
func (c *slackAPIClient) postNote(ctx context.Context, channel, text, threadID string) (string, error) {
	return c.postContextMessage(ctx, channel, threadID, truncateRunes(text, slackSectionTextMax))
}

// postContextMessage posts text as a context block, Slack's small muted text,
// with the same text as the notification fallback.
func (c *slackAPIClient) postContextMessage(ctx context.Context, channel, threadID, text string) (string, error) {
	body := map[string]any{
		paramChannel: channel,
		paramText:    text,
		paramBlocks:  []any{contextBlock(text)},
	}
	if threadID != "" {
		body[paramThreadTS] = threadID
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// encodeSignInValue and decodeSignInValue carry a channel sign-in prompt's
// thread and prompt ID in its button value. A thread ts never holds "|".
func encodeSignInValue(threadID, promptID string) string { return threadID + "|" + promptID }

func decodeSignInValue(value string) (threadID, promptID string, ok bool) {
	threadID, promptID, ok = strings.Cut(value, "|")
	return threadID, promptID, ok && promptID != ""
}

// postSignInPrompt posts the sign-in prompt as a real threaded message and
// returns its ts. It is the DM form of the prompt: a DM thread has one reader,
// so nothing is hidden by making it ephemeral, and only thread replies render
// in the assistant pane. The returned ts lets the prompt be rewritten in place
// once the link completes.
func (c *slackAPIClient) postSignInPrompt(ctx context.Context, channel, threadID, linkURL string, trigger signInTrigger) (string, error) {
	return c.postJSON(ctx, methodChatPostMessage, signInPromptBody(channel, threadID, linkURL, "", false, trigger))
}

// postSignInPromptEphemeral posts the sign-in prompt visible to user only. It
// is the channel form: the link is minted for one identity, so a thread full
// of bystanders must not see it (klaus-gateway#185). An ephemeral has no
// addressable ts: the caller replaces it through its Sign in click's
// response_url, or confirms the completed link with a fresh ephemeral when
// no click reached this process. promptID names this card in the button's
// value. Slack only surfaces a thread-scoped ephemeral in a thread that
// already shows a message, which is why the caller anchors a thread notice
// first (klaus-gateway#156). supersedes marks a prompt that replaces one whose
// link expired.
func (c *slackAPIClient) postSignInPromptEphemeral(ctx context.Context, channel, threadID, user, linkURL, promptID string, supersedes bool, trigger signInTrigger) error {
	body := signInPromptBody(channel, threadID, linkURL, promptID, supersedes, trigger)
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
// allowed?" prompt with Allow/Decline buttons. Only the initiator receives it, so only
// the initiator can click. The button value encodes the thread and the newcomer
// so the interaction handler resolves the right parked request.
func (c *slackAPIClient) postAccessConsentPrompt(ctx context.Context, channel, threadID, initiator, newcomer string) error {
	// A thread is one conversation, the initiator's. A granted newcomer's turns
	// run under their own sign-in, but in that conversation: they see what the
	// agent was told and did so far, and steer it from here on.
	text := fmt.Sprintf("*<@%s> wants to join this thread*\nThey would see this conversation and instruct the agent in it, under their own sign-in. Allow them?", newcomer)
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
						bkText:     map[string]any{bkType: bkPlainText, bkText: "Allow"},
						bkStyle:    bkPrimary,
						bkActionID: accessAllow,
						bkValue:    value,
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "Decline"},
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
	resp, err := c.postJSONResponse(ctx, method, body)
	return resp.Ts, err
}

// postJSONResponse is postJSON answering with the whole response: the
// conversation a message landed in as well as its ts. A direct message is
// addressed by the person's user ID, and only the response names the D…
// channel that later edits of the message need.
func (c *slackAPIClient) postJSONResponse(ctx context.Context, method string, body any) (slackResponse, error) {
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
		return slackResponse{}, fmt.Errorf("slack %s: marshal: %w", method, err)
	}
	resp, err := c.sendResponse(ctx, method, "application/json; charset=utf-8", string(data))
	if branded && identityRejectedErr(err) {
		c.noteIdentityRejected(err)
		payload, _ = build(false)
		if data, merr := json.Marshal(payload); merr == nil {
			return c.sendResponse(ctx, method, "application/json; charset=utf-8", string(data))
		}
	}
	return resp, err
}

type slackResponse struct {
	OK      bool   `json:"ok"`
	Ts      string `json:"ts"`
	Channel string `json:"channel,omitempty"`
	Error   string `json:"error,omitempty"`
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
	resp, err := c.sendResponse(ctx, method, contentType, payload)
	return resp.Ts, err
}

// sendResponse is send answering with the decoded response.
func (c *slackAPIClient) sendResponse(ctx context.Context, method, contentType, payload string) (slackResponse, error) {
	body, err := c.call(ctx, method, contentType, payload)
	if err != nil {
		return slackResponse{}, err
	}
	var result slackResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return slackResponse{}, fmt.Errorf("slack %s: decode response: %w", method, err)
	}
	if !result.OK {
		return slackResponse{}, &apiError{method: method, code: result.Error}
	}
	return result, nil
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
			// Debug, not Warn: a retried 429 is routine pacing, and the very
			// condition this records is when a Warn would flood the log. The
			// counter is the signal; this line is for diagnosing one call.
			if c.logger != nil {
				c.logger.Debug("slack: rate limited", "method", method, "wait", wait, "attempt", attempt)
			}
			if attempt >= maxAttempts || wait > rateLimitRetryCap {
				c.noteRateLimited(method, rateLimitExhausted)
				return nil, fmt.Errorf("slack %s: rate limited (retry after %s)", method, wait)
			}
			select {
			case <-ctx.Done():
				// The wait was cut short by the turn's cancellation, so no
				// retry happens and nothing is counted: one lost sample on a
				// turn that is going away anyway.
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			c.noteRateLimited(method, rateLimitRetried)
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
