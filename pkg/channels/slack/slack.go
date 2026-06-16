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
	"sync"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
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

	gw             channels.Gateway
	started        atomic.Bool
	evHandler      http.Handler
	ixHandler      http.Handler // interactions endpoint; nil in socketmode

	accessMu sync.Mutex
	access   map[string]*AccessState // keyed by threadID

	turnsMu sync.Mutex
	turns   map[string]context.CancelFunc // keyed by threadID; cancels in-flight SendCompletion

	pendingMu    sync.Mutex
	pendingTasks map[string]*pendingTask // keyed by threadID
}

// pendingTask records the A2A task paused at input-required for a thread.
type pendingTask struct {
	TaskID    string
	AgentRef  string
	Channel   string // Slack channel ID for posting the resumed response
	ChannelID string // logical channel ID used in the routing key
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

	a.started.Store(true)
	return nil
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
	case cmdOpen:
		state.Mode = ModeOpen
	case cmdObserve:
		state.Mode = ModeLocked
		state.Observe = true
	default:
		state.Mode = ModeLocked
	}
	if len(a.AllowedUsers) > 0 {
		state.Owner = a.AllowedUsers[0]
		if len(a.AllowedUsers) > 1 {
			state.Allowed = make(map[string]bool, len(a.AllowedUsers)-1)
			for _, u := range a.AllowedUsers[1:] {
				state.Allowed[u] = true
			}
			state.Mode = ModeSelective
		}
	} else {
		state.Owner = ownerID
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

	slackUser := msg.Subject // Slack user ID before email resolution

	state := a.getAccess(msg.ThreadID, slackUser)
	if !state.Deliver(slackUser) {
		return nil
	}
	respond := state.Permitted(slackUser)

	msg.AgentRef = a.DefaultAgent

	// Resume a paused input-required task when one exists for this thread.
	if task := a.takePendingTask(msg.ThreadID); task != nil {
		msg.TaskID = task.TaskID
	}

	if msg.Subject != "" {
		client := a.apiClient()
		email, err := client.lookupUserEmail(ctx, msg.Subject)
		if err != nil {
			a.Logger.Warn("slack: user email lookup failed, falling back to user ID", "user", msg.Subject, "error", err)
		} else if email != "" {
			msg.Subject = email
		}
	}

	ref, err := a.gw.Resolve(ctx, msg)
	if err != nil {
		return fmt.Errorf("slack: resolve: %w", err)
	}

	// Register an in-flight turn so /stop can cancel it.
	turnCtx, turnCancel := context.WithCancel(ctx)
	defer turnCancel()
	a.turnsMu.Lock()
	if a.turns == nil {
		a.turns = make(map[string]context.CancelFunc)
	}
	a.turns[msg.ThreadID] = turnCancel
	a.turnsMu.Unlock()
	defer func() {
		a.turnsMu.Lock()
		delete(a.turns, msg.ThreadID)
		a.turnsMu.Unlock()
	}()

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		return fmt.Errorf("slack: send completion: %w", err)
	}

	// Observe mode: forward the turn to the agent for context but suppress the reply.
	if !respond {
		for range deltas {
		}
		return nil
	}

	client := a.apiClient()
	ts, err := client.postMessage(ctx, slackChannel, "_thinking…_", msg.ThreadID)
	if err != nil {
		return fmt.Errorf("slack: post placeholder: %w", err)
	}

	w := newBatchedWriterWithClient(client, slackChannel, ts, msg.ThreadID)
	if err := w.run(ctx, deltas); err != nil {
		return err
	}

	// If the agent paused for user input, post a Block Kit approval prompt and
	// register the pending task so the next message resumes it.
	if w.promptDelta != nil {
		pd := w.promptDelta
		task := &pendingTask{
			TaskID:    pd.TaskID,
			AgentRef:  msg.AgentRef,
			Channel:   slackChannel,
			ChannelID: msg.ChannelID,
		}
		a.storePendingTask(msg.ThreadID, task)
		return client.postApprovalPrompt(ctx, slackChannel, msg.ThreadID, pd.Content, pd.TaskID)
	}
	return nil
}

// slackInnerEvent is the inner event object present in both Events API
// and Socket Mode payloads.
type slackInnerEvent struct {
	Type     string `json:"type"`
	SubType  string `json:"subtype,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	User     string `json:"user"`
	Text     string `json:"text"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
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
		UserID:    "",     // thread-scoped session: all participants share one contextID
		Subject:   e.User, // Slack user ID forwarded for access control and downstream identity
		ThreadID:  threadID,
		Text:      text,
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
