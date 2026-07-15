// Package slack is the Slack channel adapter for klaus-gateway.
//
// Two connection modes are supported:
//   - events: Slack Events API HTTP webhook (production).
//   - socketmode: Slack Socket Mode WebSocket (development).
//
// The adapter is disabled by default; set --slack-enabled (or
// KLAUS_GATEWAY_SLACK_ENABLED=true) to activate it.
package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// ChannelName identifies the Slack adapter in routing keys.
const ChannelName = "slack"

// OBOTokenSource mints a fresh per-request human muster access token for a
// linked Slack user and drives the account-linking UX. *musterlink.Linker
// satisfies it. When the adapter's OBO field is non-nil (linking enabled), the
// human token is the only credential forwarded to the agent: a turn without one
// is aborted rather than degraded to the gateway service account (see
// klaus-gateway#116). When OBO is nil (linking disabled), there is no human
// path and turns run as the M2M ServiceAccount identity (the historical
// behaviour) via the gateway's ForwardedTokenSource fallback.
type OBOTokenSource interface {
	// TokenFor returns a fresh human muster access token for the Slack user,
	// or musterlink.ErrNotLinked when the user has not linked an identity.
	TokenFor(ctx context.Context, slackUserID string) (string, error)
	// LinkURL returns the absolute "Sign in to Giant Swarm" URL that starts the
	// account-linking flow for the Slack user (signed, single-use state).
	LinkURL(slackUserID string) string
	// Unlink removes any stored link for the Slack user (the /klaus logout path).
	Unlink(slackUserID string)
}

// Mode constants for the Slack connection method.
const (
	ModeEvents     = "events"
	ModeSocketMode = "socketmode"
)

// Adapter implements channels.ChannelAdapter for the Slack channel.
type Adapter struct {
	Logger  *slog.Logger
	Mode    string
	Secrets Secrets
	// DefaultAgent is the agentRef every Slack thread routes to. Must be
	// non-empty; Start returns an error when it is unset.
	DefaultAgent string
	// APIBase overrides the Slack Web API base URL. Empty uses the default
	// (https://slack.com/api). Set in tests to point at a fake server.
	APIBase string
	// DefaultAccessMode sets the initial access mode for new threads.
	// "open" or "observe"; empty/"locked" means ModeLocked (owner-only).
	DefaultAccessMode string
	// AllowedUsers is a static allow-list of Slack user IDs seeded into every
	// new thread's AccessState. The first entry becomes owner when non-empty.
	AllowedUsers []string
	// DMOnly restricts the adapter to direct messages (channel_type "im").
	// When true, channel messages and @-mentions in channels are ignored, so
	// the bot only ever answers in a 1:1 DM. Default false (channels served).
	DMOnly bool
	// DropStaleEvents, when true, ignores events whose Slack ts predates this
	// process. Socket Mode can redeliver events that were queued/unacked while
	// a consumer was disconnected, so without this a restart replays — and
	// re-answers — old messages. Default false (no time-based filtering).
	DropStaleEvents bool
	// OBO, when set, mints a fresh human muster token per turn for linked Slack
	// users so the agent acts on behalf of the human. Nil disables OBO; turns
	// then run as the M2M ServiceAccount identity.
	OBO OBOTokenSource

	gw        channels.Gateway
	started   atomic.Bool
	startUnix int64 // process start; events older than this are dropped on reconnect
	evHandler http.Handler
	ixHandler http.Handler // interactions endpoint; nil in socketmode

	accessMu sync.Mutex
	access   map[string]*AccessState // keyed by threadID

	turnsMu sync.Mutex
	turns   map[string]*turn // keyed by threadID; cancels in-flight SendCompletion

	pendingMu    sync.Mutex
	pendingTasks map[string]*pendingTask // keyed by threadID

	emailMu    sync.Mutex
	emailCache map[string]emailEntry // Slack user ID -> resolved email
}

// emailEntry is a cached Slack user email with its expiry.
type emailEntry struct {
	email   string
	expires time.Time
}

// userEmailCacheTTL bounds how long a resolved Slack email is reused. Emails are
// effectively static, so a long TTL keeps users.info (a Tier-4 rate-limited
// endpoint) off the per-turn hot path while still picking up the rare change.
const userEmailCacheTTL = time.Hour

// turn is an in-flight agent turn. The pointer identity lets dispatch clean up
// only its own registry entry, even if a later turn on the same thread has
// already replaced it. Only the most recently started turn per thread is
// registered, so /stop cancels that one; threads are effectively serialized
// per conversation, so an older overlapping turn is not the expected case.
type turn struct {
	cancel context.CancelFunc
}

// pendingTask records the A2A task paused at input-required for a thread.
type pendingTask struct {
	TaskID    string
	AgentRef  string
	Channel   string // Slack channel ID for posting the resumed response
	ChannelID string // logical channel ID used in the routing key
	// Prompt is the structured approval request the task is paused on, used to
	// map a free-text reply or choice click back to a HITL decision.
	Prompt *channels.HitlPrompt
}

// Name returns the channel name used in routing keys.
func (a *Adapter) Name() string { return ChannelName }

// Start wires the Gateway facade and initialises the chosen connection mode.
func (a *Adapter) Start(ctx context.Context, gw channels.Gateway) error {
	if gw == nil {
		return errors.New("slack: nil gateway")
	}
	if a.DefaultAgent == "" {
		return errors.New("slack: DefaultAgent must be set")
	}
	switch a.DefaultAccessMode {
	case "", accessModeLocked, accessModeOpen, accessModeObserve:
	default:
		return fmt.Errorf("slack: unknown DefaultAccessMode %q: want %s, %s, or %s",
			a.DefaultAccessMode, accessModeLocked, accessModeOpen, accessModeObserve)
	}
	if a.Logger == nil {
		a.Logger = slog.Default()
	}
	a.gw = gw

	switch a.Mode {
	case ModeEvents, "":
		a.evHandler = &eventsHandler{
			signingSecret: a.Secrets.SigningSecret,
			botToken:      a.Secrets.BotToken,
			adapter:       a,
			logger:        a.Logger,
			ctx:           ctx,
		}
		a.ixHandler = &interactionsHandler{
			signingSecret: a.Secrets.SigningSecret,
			adapter:       a,
			ctx:           ctx,
		}
	case ModeSocketMode:
		if a.Secrets.AppToken == "" {
			return errors.New("slack: app_token is required in socketmode")
		}
		sm := &socketModeClient{
			appToken: a.Secrets.AppToken,
			botToken: a.Secrets.BotToken,
			adapter:  a,
			logger:   a.Logger,
		}
		go sm.run(ctx)
	default:
		return fmt.Errorf("slack: unknown mode %q: want %q or %q", a.Mode, ModeEvents, ModeSocketMode)
	}

	a.startUnix = time.Now().Unix()
	a.started.Store(true)
	return nil
}

// acceptEvent decides whether an inbound Slack event should be processed at
// all, before any access-control or command handling. It enforces two guards:
//
//   - DM-only: when DMOnly is set, anything that is not a direct message
//     (channel_type "im" / a "D…" channel ID) is dropped, so the bot answers
//     only in 1:1 DMs and never in a public channel.
//   - Staleness: events whose Slack ts predates this process are dropped.
//     Socket Mode redelivers events queued while a consumer was disconnected,
//     so without this a gateway restart replays — and re-answers — old
//     messages. New messages (ts >= start) always pass.
func (a *Adapter) acceptEvent(inner slackInnerEvent) bool {
	if a.DMOnly && inner.ChannelType != "im" && !strings.HasPrefix(inner.Channel, "D") {
		a.Logger.Debug("slack: ignoring non-DM event (DM-only mode)",
			"channel", inner.Channel, "channel_type", inner.ChannelType)
		return false
	}
	if a.DropStaleEvents && a.startUnix > 0 {
		if sec := eventUnix(inner.TS); sec > 0 && sec < a.startUnix {
			a.Logger.Info("slack: dropping stale event from before gateway start",
				"event_ts", inner.TS, "channel", inner.Channel)
			return false
		}
	}
	return true
}

// eventUnix parses the integer second component of a Slack ts ("123.456").
// Returns 0 when the value is missing or unparseable.
func eventUnix(ts string) int64 {
	if ts == "" {
		return 0
	}
	if dot := strings.IndexByte(ts, '.'); dot >= 0 {
		ts = ts[:dot]
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return 0
	}
	return sec
}

// Stop marks the adapter as stopped. The context passed to Start is the
// primary shutdown mechanism for background goroutines.
func (a *Adapter) Stop(_ context.Context) error {
	a.started.Store(false)
	return nil
}

// Mount attaches /channels/slack/events and /channels/slack/interactions to r.
// No-op in socketmode (no HTTP handlers needed).
func (a *Adapter) Mount(r chi.Router) {
	if a.evHandler == nil {
		return
	}
	r.Route("/channels/slack", func(r chi.Router) {
		r.Handle("/events", a.evHandler)
		if a.ixHandler != nil {
			r.Handle("/interactions", a.ixHandler)
		}
	})
}

// LookupUserEmail returns the Slack-workspace-verified email for a Slack user
// ID. It is the SlackEmail lookup the OBO linker uses at callback to enforce
// that the linked muster identity's email matches the Slack user's (anti-spoof).
func (a *Adapter) LookupUserEmail(ctx context.Context, slackUserID string) (string, error) {
	return a.cachedUserEmail(ctx, slackUserID)
}

// cachedUserEmail resolves a Slack user ID to an email, memoizing successful
// lookups for userEmailCacheTTL. Errors are not cached so a transient failure
// is retried on the next turn.
func (a *Adapter) cachedUserEmail(ctx context.Context, slackUserID string) (string, error) {
	now := time.Now()
	a.emailMu.Lock()
	if e, ok := a.emailCache[slackUserID]; ok && now.Before(e.expires) {
		a.emailMu.Unlock()
		return e.email, nil
	}
	a.emailMu.Unlock()

	email, err := a.apiClient().lookupUserEmail(ctx, slackUserID)
	if err != nil {
		return "", err
	}
	a.emailMu.Lock()
	if a.emailCache == nil {
		a.emailCache = make(map[string]emailEntry)
	}
	a.emailCache[slackUserID] = emailEntry{email: email, expires: now.Add(userEmailCacheTTL)}
	a.emailMu.Unlock()
	return email, nil
}

// resolveSubjectEmail replaces the Slack user ID in msg.Subject with the user's
// workspace email when it can be resolved, so downstream identity claims carry
// the email rather than the opaque Slack ID. A lookup failure leaves the ID in
// place (logged, never fatal). Shared by the message-dispatch and button-click
// paths.
func (a *Adapter) resolveSubjectEmail(ctx context.Context, msg *channels.InboundMessage) {
	if msg.Subject == "" {
		return
	}
	email, err := a.cachedUserEmail(ctx, msg.Subject)
	if err != nil {
		a.Logger.Warn("slack: user email lookup failed, falling back to user ID", "user", msg.Subject, "error", err)
		return
	}
	if email != "" {
		msg.Subject = email
	}
}

// apiClient returns a Slack Web API client using the adapter's bot token
// and the optional test-override base URL.
func (a *Adapter) apiClient() *slackAPIClient {
	base := a.APIBase
	if base == "" {
		base = slackAPIBase
	}
	return &slackAPIClient{botToken: a.Secrets.BotToken, baseURL: base}
}

// postSignIn posts the ephemeral "Sign in to Giant Swarm" prompt for the
// account-linking flow. It is driven by the explicit /klaus login command and
// by an unlinked user's first turn (which is aborted, not run as the SA). A
// failure to post is logged and swallowed.
func (a *Adapter) postSignIn(ctx context.Context, slackChannel, threadID, slackUser string) {
	url := a.OBO.LinkURL(slackUser)
	if url == "" {
		a.Logger.Warn("slack: empty sign-in link URL, skipping prompt", "user", slackUser)
		return
	}
	if err := a.apiClient().postSignInPrompt(ctx, slackChannel, threadID, slackUser, url); err != nil {
		a.Logger.Warn("slack: post sign-in prompt failed", "user", slackUser, "error", err)
	}
}

// getAccess returns (or lazily creates) the AccessState for a thread.
// ownerID is the Slack user ID of the message sender; it becomes the thread
// owner if this is the first message in the thread.
func (a *Adapter) getAccess(threadID, ownerID string) *AccessState {
	a.accessMu.Lock()
	defer a.accessMu.Unlock()
	if a.access == nil {
		a.access = make(map[string]*AccessState)
	}
	if state, ok := a.access[threadID]; ok {
		return state
	}
	state := &AccessState{}
	switch a.DefaultAccessMode {
	case accessModeOpen:
		state.mode = ModeOpen
	case accessModeObserve:
		state.mode = ModeLocked
		state.observe = true
	default:
		state.mode = ModeLocked
	}
	// A static allow-list overrides first-sender ownership: the first entry
	// becomes owner, any remaining entries seed the selective allow-list.
	if len(a.AllowedUsers) > 0 {
		state.owner = a.AllowedUsers[0]
		if len(a.AllowedUsers) > 1 {
			state.allowed = make(map[string]bool, len(a.AllowedUsers)-1)
			for _, u := range a.AllowedUsers[1:] {
				state.allowed[u] = true
			}
			state.mode = ModeSelective
		}
	} else {
		state.owner = ownerID
	}
	a.access[threadID] = state
	return state
}

// isActiveThread reports whether the bot has an active session in threadID —
// either an access state (meaning it was mentioned at some point) or a pending
// input-required task. Used to decide whether to route message.channels thread
// replies without requiring an @-mention.
func (a *Adapter) isActiveThread(threadID string) bool {
	a.accessMu.Lock()
	_, hasAccess := a.access[threadID]
	a.accessMu.Unlock()
	if hasAccess {
		return true
	}
	return a.hasPendingTask(threadID)
}

// storePendingTask records a paused input-required task for a thread.
// Any existing pending task for that thread is replaced.
func (a *Adapter) storePendingTask(threadID string, task *pendingTask) {
	a.pendingMu.Lock()
	if a.pendingTasks == nil {
		a.pendingTasks = make(map[string]*pendingTask)
	}
	a.pendingTasks[threadID] = task
	a.pendingMu.Unlock()
}

// takePendingTask atomically retrieves and removes a pending task for a thread.
// Returns nil when no task is pending.
func (a *Adapter) takePendingTask(threadID string) *pendingTask {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	task := a.pendingTasks[threadID]
	delete(a.pendingTasks, threadID)
	return task
}

// hasPendingTask reports whether a thread has a pending input-required task.
func (a *Adapter) hasPendingTask(threadID string) bool {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	_, ok := a.pendingTasks[threadID]
	return ok
}

// handleInbound runs the shared inbound pipeline for one Slack event:
// accept-gate, normalise, active-thread gate (for channel thread replies),
// command handling, then dispatch. Both transports (the Events API HTTP
// handler and the Socket Mode reader) call it so the two behave identically.
// ctx is the adapter-lifecycle context.
func (a *Adapter) handleInbound(ctx context.Context, inner slackInnerEvent) {
	if !a.acceptEvent(inner) {
		return
	}
	threadReplyOnly := inner.threadReplyOnly()
	msg, ok := inner.toInboundMessage(threadReplyOnly)
	if !ok {
		return
	}
	if threadReplyOnly && !a.isActiveThread(msg.ThreadID) {
		return
	}
	if cmd := parseCommand(msg.Text); cmd != nil {
		if a.handleCommand(ctx, cmd, msg.Subject, inner.Channel, msg.ThreadID) {
			return
		}
	}
	if err := a.dispatch(ctx, msg, inner.Channel); err != nil {
		if !errors.Is(err, context.Canceled) {
			a.Logger.Error("slack: dispatch error", "channel", inner.Channel, "error", err)
		}
	}
}

// dispatch resolves an inbound Slack message to a Klaus instance, posts a
// placeholder reply in-thread, and streams the completion back via chat.update batches.
func (a *Adapter) dispatch(ctx context.Context, msg channels.InboundMessage, slackChannel string) error {
	if !a.started.Load() {
		return errors.New("slack: adapter not started")
	}

	slackUser := msg.Subject // raw Slack user ID; keys access control

	state := a.getAccess(msg.ThreadID, slackUser)
	if !state.Deliver(slackUser) {
		return nil
	}
	respond := state.Permitted(slackUser)

	msg.AgentRef = a.DefaultAgent

	// Resume a paused input-required task when one exists for this thread.
	// Map the typed reply to a structured HITL decision so the paused tool
	// confirmation is actually resolved (a plain text reply would leave the
	// tool call dangling and corrupt the model history).
	if task := a.takePendingTask(msg.ThreadID); task != nil {
		msg.TaskID = task.TaskID
		msg.Decision = decisionFromText(task.Prompt, msg.Text)
	}

	a.resolveSubjectEmail(ctx, &msg)

	token, ok := a.humanToken(ctx, slackChannel, msg.ThreadID, slackUser, respond)
	if !ok {
		return nil
	}
	msg.BearerToken = token

	ref, err := a.gw.Resolve(ctx, msg)
	if err != nil {
		return fmt.Errorf("slack: resolve: %w", err)
	}

	turnCtx, done := a.registerTurn(ctx, msg.ThreadID)
	defer done()

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		return fmt.Errorf("slack: send completion: %w", err)
	}

	// Observe mode: forward the turn to the agent for context but suppress the
	// Slack reply. Drain the stream so the producer completes, surfacing any
	// upstream error in the log rather than discarding it silently.
	if !respond {
		for d := range deltas {
			if d.Err != nil {
				a.Logger.Warn("slack: observe-mode turn failed", "thread", msg.ThreadID, "error", d.Err)
			}
		}
		return nil
	}

	return a.streamResponse(ctx, a.apiClient(), deltas, msg, slackChannel, msg.ThreadID, "_thinking…_")
}

// humanToken mints the linked Slack user's muster token for a turn. When
// linking is disabled (OBO nil) it returns ("", true): there is no human path
// and the turn proceeds with no per-user credential. When linking is enabled,
// the human's muster token is the only credential forwarded (the agent always
// acts as the human, never as the gateway service account), so a turn without
// a valid one returns ok=false and the caller aborts, not silently degraded to
// the M2M SA identity, which is confusing, masks failures, and is a privilege
// risk (klaus-gateway#116):
//   - unlinked user  -> prompt sign-in and stop;
//   - transient error -> surface it and stop.
//
// Both prompts are posted only when respond is true, so only a user we would
// actually respond to is messaged (a DM sender is always permitted;
// observe-mode onlookers stay silent). Shared by the message-dispatch and
// button-click paths.
func (a *Adapter) humanToken(ctx context.Context, slackChannel, threadID, slackUser string, respond bool) (string, bool) {
	if a.OBO == nil {
		return "", true
	}
	if slackUser == "" {
		a.Logger.Warn("slack: aborting turn without a slack user (no human token possible)")
		return "", false
	}
	token, err := a.OBO.TokenFor(ctx, slackUser)
	switch {
	case err == nil:
		return token, true
	case errors.Is(err, musterlink.ErrNotLinked):
		a.Logger.Debug("slack: link unavailable (unlinked or refresh token dead), prompting sign-in", "user", slackUser)
		if respond {
			a.postSignIn(ctx, slackChannel, threadID, slackUser)
		}
	default:
		a.Logger.Warn("slack: human token unavailable, aborting turn", "user", slackUser, "error", err)
		if respond {
			const msgText = "I couldn't refresh your Giant Swarm sign-in just now. Please try again in a moment; if it keeps failing, re-link with the `login` command (type it with a leading space)."
			if _, perr := a.apiClient().postMessage(ctx, slackChannel, msgText, threadID); perr != nil {
				a.Logger.Warn("slack: post token-error message failed", "user", slackUser, "error", perr)
			}
		}
	}
	return "", false
}

// registerTurn installs a cancelable in-flight turn for threadID so /stop can
// cancel it, and returns the turn context plus a cleanup func that cancels the
// turn and removes only this turn's registry entry (even if a later turn on the
// same thread has already replaced it).
func (a *Adapter) registerTurn(ctx context.Context, threadID string) (context.Context, func()) {
	turnCtx, cancel := context.WithCancel(ctx)
	t := &turn{cancel: cancel}
	a.turnsMu.Lock()
	if a.turns == nil {
		a.turns = make(map[string]*turn)
	}
	a.turns[threadID] = t
	a.turnsMu.Unlock()
	return turnCtx, func() {
		cancel()
		a.turnsMu.Lock()
		if a.turns[threadID] == t {
			delete(a.turns, threadID)
		}
		a.turnsMu.Unlock()
	}
}

// streamResponse posts a placeholder message, streams deltas into it via
// batched chat.update calls, and — when the agent pauses for input — registers
// the pending task and posts the HITL prompt. Shared by dispatch (a new turn)
// and handleDecision (a button-click resume) so both render identically.
func (a *Adapter) streamResponse(ctx context.Context, client *slackAPIClient, deltas <-chan channels.OutboundDelta, msg channels.InboundMessage, slackChannel, threadID, placeholder string) error {
	ts, err := client.postMessage(ctx, slackChannel, placeholder, threadID)
	if err != nil {
		return fmt.Errorf("slack: post placeholder: %w", err)
	}

	w := newBatchedWriterWithClient(client, slackChannel, ts, threadID, a.Logger)
	if err := w.run(ctx, deltas); err != nil {
		return err
	}

	if w.promptDelta != nil {
		pd := w.promptDelta
		a.storePendingTask(threadID, &pendingTask{
			TaskID:    pd.TaskID,
			AgentRef:  msg.AgentRef,
			Channel:   slackChannel,
			ChannelID: msg.ChannelID,
			Prompt:    pd.Prompt,
		})
		return a.postHitlPrompt(ctx, client, slackChannel, threadID, pd)
	}
	return nil
}

// slackInnerEvent is the inner event object present in both Events API
// and Socket Mode payloads.
type slackInnerEvent struct {
	Type        string `json:"type"`
	SubType     string `json:"subtype,omitempty"`
	BotID       string `json:"bot_id,omitempty"`
	User        string `json:"user"`
	Text        string `json:"text"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type,omitempty"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts,omitempty"`
}

// isDM reports whether the event originated in a 1:1 direct message. Slack sets
// channel_type "im" for DMs and uses a "D…" channel ID; either is sufficient.
func (e slackInnerEvent) isDM() bool {
	return e.ChannelType == "im" || strings.HasPrefix(e.Channel, "D")
}

// threadReplyOnly reports whether this event may only be handled as a thread
// reply to an already-active bot thread. A plain "message" in a channel (not a
// DM) qualifies: it must never trigger the bot on its own, so top-level channel
// chatter is dropped by toInboundMessage and only thread replies survive (then
// gated on active-thread state by the caller). app_mention and DMs return false
// and always pass through.
func (e slackInnerEvent) threadReplyOnly() bool {
	return e.Type == evtMessage && !e.isDM()
}

// toInboundMessage maps a Slack inner event to the normalised InboundMessage.
// Returns false when the event should be ignored (bot message, empty text, …).
// threadReplyOnly, when true, accepts only thread reply messages (thread_ts set
// and different from ts); used for message.channels events where we only want
// to route replies to existing bot threads.
func (e slackInnerEvent) toInboundMessage(threadReplyOnly bool) (channels.InboundMessage, bool) {
	if e.BotID != "" || e.SubType != "" {
		return channels.InboundMessage{}, false
	}
	// An event without a Slack user has no subject: it cannot be
	// access-controlled or attributed, so it must never become a turn.
	if e.User == "" {
		return channels.InboundMessage{}, false
	}
	switch e.Type {
	case evtAppMention, evtMessage:
	default:
		return channels.InboundMessage{}, false
	}
	threadID := e.ThreadTS
	if threadID == "" {
		threadID = e.TS
	}
	if threadReplyOnly && e.ThreadTS == "" {
		return channels.InboundMessage{}, false
	}
	text := StripMention(e.Text)
	if text == "" {
		return channels.InboundMessage{}, false
	}
	return channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: e.Channel,
		UserID:    "", // thread-scoped session: all participants share one contextID
		ThreadID:  threadID,
		Text:      text,
		// Subject carries the raw Slack user ID. It keys per-thread access
		// control only; mapping it to an email/OAuth sub for downstream
		// identity is deferred to the auth phase that actually consumes it.
		Subject: e.User,
	}, true
}

// StripMention removes leading <@USERID> tokens that Slack injects into
// app_mention event text.
func StripMention(text string) string {
	s := text
	for len(s) > 0 && s[0] == '<' {
		end := 0
		for end < len(s) && s[end] != '>' {
			end++
		}
		if end >= len(s) {
			break
		}
		s = s[end+1:]
		// Consume optional trailing space.
		if len(s) > 0 && s[0] == ' ' {
			s = s[1:]
		}
	}
	return s
}
