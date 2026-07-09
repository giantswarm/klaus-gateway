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
	"github.com/giantswarm/klaus-gateway/pkg/channels/slack/classifier"
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
	// Models, when set, resolves the default agent's model id for /usage.
	// Nil omits the model line.
	Models AgentModelSource

	// ProgressMode selects how turn progress is shown: "auto" (default; reactions
	// with a text fallback when reactions:write is unavailable), "reactions", or
	// "text".
	ProgressMode string
	// WorkingEmoji, DoneEmoji, FailedEmoji override the progress reaction emoji
	// names (no surrounding colons). Empty uses the defaults.
	WorkingEmoji string
	DoneEmoji    string
	FailedEmoji  string
	// ClearReactionOnDone, when true, removes the working reaction on a
	// successful turn without adding a done reaction. When false the working
	// reaction is swapped for DoneEmoji. The failed reaction is unaffected.
	ClearReactionOnDone bool

	gw        channels.Gateway
	started   atomic.Bool
	startUnix int64 // process start; events older than this are dropped on reconnect
	evHandler http.Handler
	ixHandler http.Handler // interactions endpoint; nil in socketmode

	// Classifier auto-approves low-risk (read-only) HITL tool prompts so
	// investigation stays frictionless. Nil disables auto-approval (every prompt
	// is surfaced to the human).
	Classifier *classifier.Classifier

	accessMu sync.Mutex
	access   AccessPolicy // lazily initialised via accessPolicy()

	pendingAccessMu sync.Mutex
	pendingAccess   map[string]map[string]*pendingAccessReq // threadID -> userID -> parked request

	turnsMu sync.Mutex
	turns   map[string]*turn // keyed by threadID; cancels in-flight SendCompletion

	// reactionsUnsupported caches the auto-mode downgrade to text progress after
	// a reactions.add returns missing_scope, so later turns skip the failed call.
	reactionsUnsupported atomic.Bool

	inflightMu sync.Mutex
	inflight   map[string]struct{} // threadIDs with a turn in progress (serialization)

	pendingMu    sync.Mutex
	pendingTasks map[string]*pendingTask // keyed by threadID

	emailMu    sync.Mutex
	emailCache map[string]emailEntry // Slack user ID -> resolved email

	seenEventsMu sync.Mutex
	seenEvents   map[string]time.Time // Slack event_id -> dedup entry expiry

	// detailsMu guards details. Absent thread resolves to detailsOn (the MVP
	// default). State persists for the thread's lifetime (there is no session
	// end; resume-by-default).
	detailsMu sync.Mutex
	details   map[string]detailsLevel // keyed by threadID

	// usageMu guards both usage maps. lastTurn holds the most recent turn's
	// summed token counts; sessionTotal accumulates across the thread's turns.
	usageMu      sync.Mutex
	lastTurn     map[string]channels.TurnUsage // keyed by threadID
	sessionTotal map[string]channels.TurnUsage // keyed by threadID

	// resumeChecked records threadIDs whose resume existence-check already ran
	// this process, so the "starting fresh" notice posts at most once per thread.
	resumeMu      sync.Mutex
	resumeChecked map[string]struct{}

	// modelMu guards modelCache, the resolved model labels shown by /usage.
	modelMu    sync.Mutex
	modelCache map[string]modelEntry // agentRef -> cached model label
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
// only its own registry entry. Turns on a thread are serialized (see
// acquireThread), so /stop cancels the single registered turn.
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
	// Usage carries the paused turn's token counts so the resuming turn reports
	// the whole turn, not just its tail.
	Usage channels.TurnUsage

	storedAt time.Time // set by storePendingTask; drives the TTL sweep
}

// pendingAccessReq is a newcomer's message parked while the thread initiator is
// asked to approve them. It is replayed through dispatch on approval.
type pendingAccessReq struct {
	msg          channels.InboundMessage
	slackChannel string

	storedAt time.Time // set by storePendingAccess; drives the TTL sweep
}

// pendingTTL bounds how long a parked task or access request is kept. Both
// maps hold user content and grow per thread; an entry this old is abandoned
// (the paused A2A task has long been resumable by nobody) and is swept on the
// next store.
const pendingTTL = 24 * time.Hour

// NewClassifier builds the HITL auto-approval risk classifier from config.
// autoApprove is the highest auto-approvable risk: "green" (default; read-only),
// "yellow", or "off". "off" returns nil, so every tool prompt is surfaced to the
// human. Red-classified calls are never auto-approvable, so "red" is rejected.
func NewClassifier(autoApprove string, allowedHosts []string) *classifier.Classifier {
	threshold, ok := parseRiskThreshold(autoApprove)
	if !ok {
		return nil
	}
	return &classifier.Classifier{Config: classifier.Config{
		AutoApproveThreshold: threshold,
		AllowedHosts:         allowedHosts,
	}}
}

// parseRiskThreshold returns the auto-approve threshold and whether a classifier
// should be built. An unrecognised value fails closed (no classifier, so every
// prompt is surfaced); config validation rejects it at startup before this runs.
func parseRiskThreshold(s string) (classifier.Risk, bool) {
	threshold, enabled, ok := classifier.ParseThreshold(s)
	if !ok {
		return 0, false
	}
	return threshold, enabled
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
	switch a.ProgressMode {
	case "", progressModeAuto, progressModeReactions, progressModeText:
	default:
		return fmt.Errorf("slack: unknown ProgressMode %q: want %s, %s, or %s",
			a.ProgressMode, progressModeAuto, progressModeReactions, progressModeText)
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

// postAccessPrompt asks the thread initiator (ephemerally) to approve a newcomer
// who wants to instruct the agent, and acks the newcomer (ephemerally) that
// their message is held pending that approval. Both posts are best-effort.
func (a *Adapter) postAccessPrompt(ctx context.Context, slackChannel, threadID, initiator, newcomer string) {
	client := a.apiClient()
	if err := client.postAccessConsentPrompt(ctx, slackChannel, threadID, initiator, newcomer); err != nil {
		a.Logger.Warn("slack: post access prompt failed", "initiator", initiator, "newcomer", newcomer, "error", err)
	}
	const ack = "Your message is waiting for the thread owner to allow you to instruct the agent here."
	if err := client.postEphemeralText(ctx, slackChannel, newcomer, threadID, ack); err != nil {
		a.Logger.Warn("slack: post access waiting-ack failed", "newcomer", newcomer, "error", err)
	}
}

// accessPolicy returns the adapter's AccessPolicy, lazily installing the
// in-memory default so direct-construction tests need no wiring.
func (a *Adapter) accessPolicy() AccessPolicy {
	a.accessMu.Lock()
	defer a.accessMu.Unlock()
	if a.access == nil {
		a.access = newMemoryAccess()
	}
	return a.access
}

// isActiveThread reports whether the bot has an active session in threadID —
// either a known initiator (it was mentioned at some point) or a pending
// input-required task. Used to decide whether to route message.channels thread
// replies without requiring an @-mention.
func (a *Adapter) isActiveThread(threadID string) bool {
	if a.accessPolicy().Initiator(threadID) != "" {
		return true
	}
	return a.hasPendingTask(threadID)
}

// storePendingAccess parks a newcomer's message while the initiator is asked to
// approve them. Hold-latest: a repeat before approval overwrites the earlier one.
func (a *Adapter) storePendingAccess(threadID, userID string, req *pendingAccessReq) {
	a.pendingAccessMu.Lock()
	defer a.pendingAccessMu.Unlock()
	if a.pendingAccess == nil {
		a.pendingAccess = make(map[string]map[string]*pendingAccessReq)
	}
	byUser := a.pendingAccess[threadID]
	if byUser == nil {
		byUser = make(map[string]*pendingAccessReq)
		a.pendingAccess[threadID] = byUser
	}
	req.storedAt = time.Now()
	byUser[userID] = req
	for thread, users := range a.pendingAccess {
		for user, r := range users {
			if time.Since(r.storedAt) > pendingTTL {
				delete(users, user)
			}
		}
		if len(users) == 0 {
			delete(a.pendingAccess, thread)
		}
	}
}

// takePendingAccess atomically retrieves and removes a parked request.
func (a *Adapter) takePendingAccess(threadID, userID string) *pendingAccessReq {
	a.pendingAccessMu.Lock()
	defer a.pendingAccessMu.Unlock()
	byUser := a.pendingAccess[threadID]
	if byUser == nil {
		return nil
	}
	req := byUser[userID]
	delete(byUser, userID)
	if len(byUser) == 0 {
		delete(a.pendingAccess, threadID)
	}
	return req
}

// storePendingTask records a paused input-required task for a thread.
// Any existing pending task for that thread is replaced. Abandoned entries are
// swept opportunistically so the map does not grow for the process lifetime.
func (a *Adapter) storePendingTask(threadID string, task *pendingTask) {
	a.pendingMu.Lock()
	if a.pendingTasks == nil {
		a.pendingTasks = make(map[string]*pendingTask)
	}
	task.storedAt = time.Now()
	a.pendingTasks[threadID] = task
	for thread, t := range a.pendingTasks {
		if time.Since(t.storedAt) > pendingTTL {
			delete(a.pendingTasks, thread)
		}
	}
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
// dedup, accept-gate, normalise, active-thread gate (for channel thread
// replies), command handling, then dispatch. Both transports (the Events API
// HTTP handler and the Socket Mode reader) call it so the two behave
// identically, including deduplication. ctx is the adapter-lifecycle context;
// eventID is the delivery's Slack event_id ("" when the transport carries none).
func (a *Adapter) handleInbound(ctx context.Context, inner slackInnerEvent, eventID string) {
	if eventID != "" && a.seenEvent(eventID) {
		a.Logger.Info("slack: dropping duplicate event delivery", "event_id", eventID)
		return
	}
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

	// Captured before the policy records this thread: true when this process has
	// no record of the thread, i.e. a reply into a thread it did not start
	// (typically after a restart): the case the resume check targets.
	firstSight := !a.isActiveThread(msg.ThreadID)

	// Access control. The first user to interact becomes the thread initiator and
	// instructs freely. A different user is gated: authenticate first (unknown
	// identity -> sign-in), then ask the initiator to approve them (the agent acts
	// under the initiator's delegated identity, so the initiator must consent).
	access := a.accessPolicy()
	initiator := access.SetInitiator(msg.ThreadID, slackUser)
	if !access.Allowed(msg.ThreadID, slackUser) {
		if a.OBO != nil && slackUser != "" {
			if _, err := a.OBO.TokenFor(ctx, slackUser); errors.Is(err, musterlink.ErrNotLinked) {
				a.Logger.Debug("slack: newcomer not linked, prompting sign-in", "user", slackUser)
				a.postSignIn(ctx, slackChannel, msg.ThreadID, slackUser)
				return nil
			}
		}
		a.storePendingAccess(msg.ThreadID, slackUser, &pendingAccessReq{msg: msg, slackChannel: slackChannel})
		a.postAccessPrompt(ctx, slackChannel, msg.ThreadID, initiator, slackUser)
		return nil
	}

	msg.AgentRef = a.DefaultAgent

	// Serialize turns per thread: a thread maps to one kagent session, and
	// concurrent turns on one session interleave its event log into incoherent
	// history. Reject a turn that arrives while another is in flight rather than
	// queueing it, so a stale follow-up is not answered minutes late. Acquire
	// before taking the pending task so a rejected turn leaves it for later.
	if !a.acquireThread(msg.ThreadID) {
		if _, perr := a.apiClient().postMessage(ctx, slackChannel, busyNotice, msg.ThreadID); perr != nil {
			a.Logger.Warn("slack: post busy notice failed", "thread", msg.ThreadID, "error", perr)
		}
		return nil
	}
	defer a.releaseThread(msg.ThreadID)

	a.resolveSubjectEmail(ctx, &msg)

	token, ok := a.humanToken(ctx, slackChannel, msg.ThreadID, slackUser, true)
	if !ok {
		return nil
	}
	msg.BearerToken = token

	// A reply into a thread this process did not start may be resuming a kagent
	// session that has since been evicted. Announce the "starting fresh"
	// degradation up front so the user is not surprised by lost context. Only for
	// replies (not a fresh root mention), at most once per thread. Advisory: never
	// aborts the turn, and bounded by a short timeout so a slow REST endpoint
	// cannot stall the first reply.
	if firstSight && msg.ThreadID != msg.MessageID {
		a.maybeAnnounceResume(ctx, msg, slackChannel)
	}

	// Resume a paused input-required task when one exists for this thread. Done
	// only after the turn is committed to run (thread slot acquired, human token
	// resolved): takePendingTask deletes the entry, so consuming it on a branch
	// that then aborts would strand the paused A2A task. Map the typed reply to a
	// structured HITL decision so the paused tool confirmation is actually
	// resolved (a plain text reply would leave the tool call dangling and corrupt
	// the model history).
	task := a.takePendingTask(msg.ThreadID)
	if task != nil {
		msg.TaskID = task.TaskID
		msg.Decision = decisionFromText(task.Prompt, msg.Text)
	}
	// A failure between the take and a running stream would otherwise strand the
	// paused A2A task (the take deleted the only handle to it); put it back so a
	// retry or button click can still resume it.
	restoreTask := func() {
		if task != nil {
			a.storePendingTask(msg.ThreadID, task)
		}
	}

	ref, err := a.gw.Resolve(ctx, msg)
	if err != nil {
		restoreTask()
		return fmt.Errorf("slack: resolve: %w", err)
	}

	turnCtx, done := a.registerTurn(ctx, msg.ThreadID)
	defer done()

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		restoreTask()
		return fmt.Errorf("slack: send completion: %w", err)
	}

	// The turn context feeds the whole stream (writer, auto-approved resumes) so
	// /stop cancels a resumed segment too, and an aborted consumer releases the
	// producer goroutine.
	var carried channels.TurnUsage
	if task != nil {
		carried = task.Usage
	}
	return a.streamResponse(turnCtx, a.apiClient(), deltas, msg, slackChannel, msg.ThreadID, msg.MessageID, thinkingPlaceholder, carried)
}

// humanToken resolves the per-turn human muster token for slackUser under the
// human-token-forwarding invariant: when linking is enabled, the human's muster
// token is the only credential we forward; the agent always acts as the human,
// never as the gateway service account. So a turn without a valid human token
// must be aborted, not silently degraded to the M2M SA identity, which is
// confusing, masks failures, and is a privilege risk (klaus-gateway#116):
//   - unlinked user  -> prompt sign-in and stop;
//   - transient error -> surface it and stop.
//
// Shared by message dispatch and the button-click resume path, so an approved
// tool call always runs under the approver's identity. Returns ok=false when
// the turn must not run; the user is only messaged when respond is true (a DM
// sender is always permitted; observe-mode onlookers stay silent). When OBO is
// disabled the token is empty and ok is true.
func (a *Adapter) humanToken(ctx context.Context, slackChannel, threadID, slackUser string, respond bool) (string, bool) {
	if a.OBO == nil || slackUser == "" {
		return "", true
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
		return "", false
	default:
		a.Logger.Warn("slack: human token unavailable, aborting turn", "user", slackUser, "error", err)
		if respond {
			const msgText = "I couldn't refresh your Giant Swarm sign-in just now. Please try again in a moment; if it keeps failing, re-link with the `login` command (type it with a leading space)."
			if _, perr := a.apiClient().postMessage(ctx, slackChannel, msgText, threadID); perr != nil {
				a.Logger.Warn("slack: post token-error message failed", "user", slackUser, "error", perr)
			}
		}
		return "", false
	}
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

// maxAutoApprovalsPerTurn caps consecutive auto-approved resumes within one
// turn. An agent looping on green-classified prompts would otherwise hold the
// thread's serialization slot indefinitely; past the cap the prompt is
// surfaced to the human like any other.
const maxAutoApprovalsPerTurn = 20

// streamResponse renders turn progress (reactions on triggerTS, or a text
// placeholder), streams deltas into a Block Kit markdown reply, and, when the
// agent pauses for input, registers the pending task and posts the HITL prompt.
// Shared by dispatch (a new turn; triggerTS is the user message) and
// handleDecision (a button-click resume; empty triggerTS uses text progress).
// ctx is the turn context (/stop cancels it); carried seeds the usage counters
// when the turn resumes a paused one so /usage reports the whole turn.
func (a *Adapter) streamResponse(ctx context.Context, client *slackAPIClient, deltas <-chan channels.OutboundDelta, msg channels.InboundMessage, slackChannel, threadID, triggerTS, placeholder string, carried channels.TurnUsage) error {
	// replyTS is the message the streamed answer edits: the text-mode placeholder,
	// or "" in reactions mode (the writer posts the first answer message lazily).
	prog, replyTS := a.startProgress(ctx, client, slackChannel, threadID, triggerTS, placeholder)

	w := newBatchedWriterWithClient(client, slackChannel, replyTS, threadID, a.detailsLevel(threadID), a.Logger)
	w.turnUsage = carried

	// cleanupCtx survives the turn context so a /stop-cancelled turn still gets
	// its progress indicator cleared and terminal notes posted.
	cleanupCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	}

	// Drive the turn to completion. A DeltaPrompt pauses the stream: a low-risk
	// (read-only) tool prompt is auto-approved and the same turn resumes in place
	// (story 14); anything else is surfaced to the human. The same writer and
	// progress indicator carry across an auto-approved resume so a multi-step turn
	// reads as one answer.
	autoApprovals := 0
	for {
		if err := w.run(ctx, deltas); err != nil {
			a.recordTurnUsage(threadID, w.turnUsage)
			cctx, cancel := cleanupCtx()
			prog.failed(cctx)
			// In text mode prog.failed is a no-op (no reaction to swap), so the
			// placeholder would linger as "thinking" with no failure signal.
			// Replace it with a terminal note, as the empty-output path does.
			// Skipped when the turn was cancelled (/stop): the cancel is intentional.
			if prog.reactTS == "" && ctx.Err() == nil {
				a.postTerminalNote(cctx, client, slackChannel, threadID, replyTS, failedNote)
			}
			cancel()
			return err
		}

		pd := w.promptDelta
		if pd == nil {
			break
		}
		w.promptDelta = nil

		if autoApprovals < maxAutoApprovalsPerTurn {
			if next, ok, err := a.maybeAutoApprove(ctx, pd, msg, threadID); ok {
				if err != nil {
					a.recordTurnUsage(threadID, w.turnUsage)
					cctx, cancel := cleanupCtx()
					prog.failed(cctx)
					cancel()
					return err
				}
				autoApprovals++
				deltas = next
				continue
			}
		}

		// Paused waiting on the user. The turn is not over: its usage so far
		// travels with the pending task so the resuming turn reports the whole
		// turn, and is recorded only when the turn actually ends.
		cctx, cancel := cleanupCtx()
		defer cancel()
		prog.clear(cctx) // drop the working indicator
		a.storePendingTask(threadID, &pendingTask{
			TaskID:    pd.TaskID,
			AgentRef:  msg.AgentRef,
			Channel:   slackChannel,
			ChannelID: msg.ChannelID,
			Prompt:    pd.Prompt,
			Usage:     w.turnUsage,
		})
		return a.postHitlPrompt(cctx, client, slackChannel, threadID, pd)
	}
	a.recordTurnUsage(threadID, w.turnUsage)
	prog.done(ctx)
	// A turn that produced no output would otherwise be silent (text mode leaves
	// the "thinking" placeholder; reactions mode shows only a done emoji with no
	// reply). Post a terminal note so the user is not left waiting.
	if !w.wroteContent() {
		a.postTerminalNote(ctx, client, slackChannel, threadID, replyTS, emptyOutputNote)
	}
	return nil
}

// postTerminalNote replaces the text-mode placeholder (replyTS) with note, or
// posts note as a new in-thread message when no placeholder exists. Best-effort:
// a failure is logged, not propagated.
func (a *Adapter) postTerminalNote(ctx context.Context, client *slackAPIClient, slackChannel, threadID, replyTS, note string) {
	if replyTS != "" {
		if err := client.chatUpdateMarkdown(ctx, slackChannel, replyTS, note); err != nil {
			a.Logger.Warn("slack: replace placeholder failed", "thread", threadID, "error", err)
		}
		return
	}
	if _, err := client.postMarkdown(ctx, slackChannel, note, threadID); err != nil {
		a.Logger.Warn("slack: post terminal note failed", "thread", threadID, "error", err)
	}
}

// maybeAutoApprove auto-approves a low-risk (read-only) tool prompt by resuming
// the paused A2A task with an approve decision, returning the resumed delta
// stream so the turn continues in place. ok is false when the prompt is not
// eligible (no classifier, an ask_user question, or above the auto-approve
// threshold), in which case the caller surfaces it to the human. When ok is true
// and err is non-nil the resume failed and the turn should abort.
func (a *Adapter) maybeAutoApprove(ctx context.Context, pd *channels.OutboundDelta, msg channels.InboundMessage, threadID string) (deltas <-chan channels.OutboundDelta, ok bool, err error) {
	if a.Classifier == nil || pd.Prompt == nil || pd.Prompt.IsAskUser() {
		return nil, false, nil
	}
	approved, res := a.Classifier.ShouldAutoApproveTool(pd.Prompt.ToolName, pd.Prompt.Args)
	if !approved {
		return nil, false, nil
	}
	a.Logger.Debug("slack: auto-approving low-risk tool prompt",
		"thread", threadID, "tool", pd.Prompt.ToolName, "risk", res.Risk.String())

	resume := msg
	resume.TaskID = pd.TaskID
	resume.Text = labelApproved
	resume.Decision = &channels.HitlDecision{Type: channels.DecisionApprove}

	ref, err := a.gw.Resolve(ctx, resume)
	if err != nil {
		return nil, true, fmt.Errorf("slack: resolve auto-approve resume: %w", err)
	}
	next, err := a.gw.SendCompletion(ctx, ref, resume)
	if err != nil {
		return nil, true, fmt.Errorf("slack: send auto-approve resume: %w", err)
	}
	return next, true, nil
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
		MessageID: e.TS, // triggering message; progress-reaction target
		Text:      text,
		// Subject carries the raw Slack user ID. It keys per-thread access
		// control only; mapping it to an email/OAuth sub for downstream
		// identity is deferred to the auth phase that actually consumes it.
		Subject: e.User,
	}, true
}

// StripMention removes leading <@USERID> tokens that Slack injects into
// app_mention event text. Only mention tokens are stripped: other leading
// angle-bracket constructs (<https://...> links, <#C...> channel refs) are
// message content and must reach the agent.
func StripMention(text string) string {
	s := text
	for strings.HasPrefix(s, "<@") {
		end := strings.IndexByte(s, '>')
		if end < 0 {
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
