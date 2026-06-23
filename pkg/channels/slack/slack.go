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

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/channels/slack/classifier"
)

// ChannelName identifies the Slack adapter in routing keys.
const ChannelName = "slack"

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
	// Classifier classifies A2A input-required prompts and auto-approves safe ones.
	// Nil disables auto-approval; all input-required events surface as Slack prompts.
	Classifier *classifier.Classifier
	// DMOnly restricts the adapter to direct messages (channel_type "im").
	// When true, channel messages and @-mentions in channels are ignored, so
	// the bot only ever answers in a 1:1 DM. Default false (channels served).
	DMOnly bool
	// DropStaleEvents, when true, ignores events whose Slack ts predates this
	// process. Socket Mode can redeliver events that were queued/unacked while
	// a consumer was disconnected, so without this a restart replays — and
	// re-answers — old messages. Default false (no time-based filtering).
	DropStaleEvents bool

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
}

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

// apiClient returns a Slack Web API client using the adapter's bot token
// and the optional test-override base URL.
func (a *Adapter) apiClient() *slackAPIClient {
	base := a.APIBase
	if base == "" {
		base = slackAPIBase
	}
	return &slackAPIClient{botToken: a.Secrets.BotToken, baseURL: base}
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

	ref, err := a.gw.Resolve(ctx, msg)
	if err != nil {
		return fmt.Errorf("slack: resolve: %w", err)
	}

	// Register an in-flight turn so /stop can cancel it.
	turnCtx, turnCancel := context.WithCancel(ctx)
	defer turnCancel()
	t := &turn{cancel: turnCancel}
	a.turnsMu.Lock()
	if a.turns == nil {
		a.turns = make(map[string]*turn)
	}
	a.turns[msg.ThreadID] = t
	a.turnsMu.Unlock()
	defer func() {
		a.turnsMu.Lock()
		if a.turns[msg.ThreadID] == t {
			delete(a.turns, msg.ThreadID)
		}
		a.turnsMu.Unlock()
	}()

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

	client := a.apiClient()
	ts, err := client.postMessage(ctx, slackChannel, "_thinking…_", msg.ThreadID)
	if err != nil {
		return fmt.Errorf("slack: post placeholder: %w", err)
	}

	w := newBatchedWriterWithClient(client, slackChannel, ts, msg.ThreadID, a.Logger)
	if err := w.run(ctx, deltas); err != nil {
		return err
	}

	// If the agent paused for user input, either auto-approve (classifier) or
	// post a Block Kit approval prompt and wait for human action.
	for w.promptDelta != nil {
		pd := w.promptDelta
		// Only generic tool approvals are eligible for auto-approval; ask_user
		// questions always need a real answer from the human.
		if a.Classifier != nil && !pd.Prompt.IsAskUser() {
			if ok, result := a.Classifier.ShouldAutoApprove(pd.Content); ok {
				a.Logger.Info("slack: auto-approving input-required", "task", pd.TaskID, "risk", result.Risk, "reason", result.Reason)
				resumeMsg := msg
				resumeMsg.TaskID = pd.TaskID
				resumeMsg.Text = labelApproved
				resumeMsg.Decision = &channels.HitlDecision{Type: channels.DecisionApprove}
				deltas, err = a.gw.SendCompletion(turnCtx, ref, resumeMsg)
				if err != nil {
					return fmt.Errorf("slack: auto-approve resume: %w", err)
				}
				w = newBatchedWriterWithClient(client, slackChannel, ts, msg.ThreadID)
				if err := w.run(ctx, deltas); err != nil {
					return err
				}
				continue
			}
		}
		task := &pendingTask{
			TaskID:    pd.TaskID,
			AgentRef:  msg.AgentRef,
			Channel:   slackChannel,
			ChannelID: msg.ChannelID,
			Prompt:    pd.Prompt,
		}
		a.storePendingTask(msg.ThreadID, task)
		return a.postHitlPrompt(ctx, client, slackChannel, msg.ThreadID, pd)
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
