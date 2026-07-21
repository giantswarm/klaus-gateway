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
	"slices"
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

// OBOTokenSource mints a fresh per-request human token (the dex id_token) for a
// linked Slack user and drives the account-linking UX. *musterlink.Linker
// satisfies it. When the adapter's OBO field is non-nil (linking enabled), the
// human token is the only credential forwarded to the agent: a turn without one
// is aborted rather than degraded to the gateway service account (see
// klaus-gateway#116). When OBO is nil (linking disabled), there is no human
// path and turns run as the M2M ServiceAccount identity (the historical
// behaviour) via the gateway's ForwardedTokenSource fallback.
type OBOTokenSource interface {
	// TokenFor returns a fresh human token (the dex id_token) for the Slack user,
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

// DMMode selects how direct messages to the bot are handled.
type DMMode string

const (
	DMModeServe    DMMode = "serve"    // answer DMs like any served surface
	DMModeRedirect DMMode = "redirect" // point the user to channels, do not answer
	DMModeIgnore   DMMode = "ignore"   // drop DM events silently
)

// ChannelMode selects which channels the bot serves.
type ChannelMode string

const (
	ChannelModeAll       ChannelMode = "all"       // every channel the bot is invited to
	ChannelModeAllowlist ChannelMode = "allowlist" // only channels in ChannelAllowlist
	ChannelModeNone      ChannelMode = "none"      // no channels (DM-only deployments)
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
	// DMMode selects how direct messages are handled: DMModeServe (answer
	// them), DMModeRedirect (point the user to channels), or DMModeIgnore
	// (drop silently). Empty means DMModeServe.
	DMMode DMMode
	// ChannelMode selects which channels are served: ChannelModeAll,
	// ChannelModeAllowlist (only ChannelAllowlist), or ChannelModeNone.
	// Empty means ChannelModeAll.
	ChannelMode ChannelMode
	// ChannelAllowlist lists the Slack channel IDs (C…) served when
	// ChannelMode is ChannelModeAllowlist.
	ChannelAllowlist []string
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
	// ConnectorPrompts enables the reactive "Connect <backend>" button: when the
	// agent reports a backend needs the user's sign-in, the adapter renders the
	// login link it relays as a button. Requires OBO.
	ConnectorPrompts bool

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

	// AgentCards resolves an agentRef to its A2A AgentCard display identity
	// (name/icon), the source of per-message agent branding. Nil disables it
	// (messages post under the app default). kagent's card carries a name but no
	// icon yet, so agent messages are named but icon-less until kagent exposes it.
	AgentCards AgentCardResolver

	gw        channels.Gateway
	baseCtx   context.Context // adapter lifecycle ctx, captured in Start; OnUserLinked's background work (login-replay dispatch and the sign-in confirmation POST) derives from it so shutdown cancels it
	started   atomic.Bool
	startUnix int64 // process start; events older than this are dropped on reconnect
	evHandler http.Handler
	ixHandler http.Handler // interactions endpoint; nil in socketmode

	accessMu sync.Mutex
	access   AccessPolicy // lazily initialised via accessPolicy()

	pendingAccessMu sync.Mutex
	pendingAccess   map[string]map[string][]*pendingAccessReq // threadID -> userID -> messages parked (in order) while the initiator decides

	pendingLoginMu sync.Mutex
	pendingLogin   map[string]map[string][]*pendingLoginReq // slackUserID -> threadID -> messages parked (in order) while the user signs in

	// signInPromptedMu guards signInPrompted, the (user, thread) pairs already
	// given a parked-message sign-in prompt within the current pendingTTL
	// window, so a burst of parked messages nudges once. Entries carry the
	// posted prompt's message coordinates so a completed link can rewrite the
	// prompt in place. Drained for a user when their link completes, so a
	// later /logout re-prompts.
	signInPromptedMu sync.Mutex
	signInPrompted   map[string]ttlEntry[signInAnchor]

	turnsMu sync.Mutex
	turns   map[string]*turn // keyed by threadID; cancels in-flight SendCompletion

	// reactionsUnsupported caches the auto-mode downgrade to text progress after
	// a reactions.add returns missing_scope, so later turns skip the failed call.
	reactionsUnsupported atomic.Bool

	inflightMu  sync.Mutex
	inflight    map[string]struct{} // threadIDs with a turn in progress (serialization)
	idleWaiters map[string][]func() // deferred replays, run when the thread slot frees

	pendingMu    sync.Mutex
	pendingTasks map[string]*pendingTask // keyed by threadID

	emailMu    sync.Mutex
	emailCache map[string]emailEntry // Slack user ID -> resolved email

	seenEventsMu sync.Mutex
	seenEvents   map[string]time.Time // Slack event_id -> dedup entry expiry

	botIDMu   sync.Mutex
	botUserID string // this bot's own Slack user ID (auth.test), cached; "" until resolved

	// dmRedirectMu guards dmRedirected, the IM channels already given the DM
	// redirect within the current dmRedirectTTL window.
	dmRedirectMu sync.Mutex
	dmRedirected map[string]ttlEntry[struct{}]

	// notServedMu guards notServedNoticed, the (channel, user) pairs already
	// given the not-served ephemeral notice within the current window, so
	// repeated mentions in an unserved channel nudge once.
	notServedMu      sync.Mutex
	notServedNoticed map[string]ttlEntry[struct{}]

	// detailsMu guards details. Absent thread resolves to detailsOn (the MVP
	// default). Entries idle past threadStateTTL are evicted.
	detailsMu sync.Mutex
	details   map[string]ttlEntry[detailsLevel] // keyed by threadID

	// usageMu guards both usage maps. threadUsage keys usage by the turn's
	// thread root; channelUsage aggregates DM channels so a top-level /usage in
	// a DM (which keys a brand-new thread) still has figures to report.
	usageMu      sync.Mutex
	threadUsage  map[string]ttlEntry[usageTotals] // keyed by threadID
	channelUsage map[string]ttlEntry[usageTotals] // keyed by Slack channel ID; DMs only

	// resumeChecked records threads whose resume existence-check ran to a
	// conclusive result, so the "starting fresh" notice posts at most once per
	// thread. Values are entry expiries (threadStateTTL).
	resumeMu      sync.Mutex
	resumeChecked map[string]time.Time

	// modelMu guards modelCache, the resolved model labels shown by /usage.
	modelMu    sync.Mutex
	modelCache map[string]modelEntry // agentRef -> cached model label

	// connectorMu guards connectorPrompted: when the connect prompt last posted
	// per (user, backend), bounding re-prompts for a backend the user neither
	// connects nor dismisses.
	connectorMu       sync.Mutex
	connectorPrompted map[string]map[string]time.Time
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
// asked to approve them. Parked per (thread, user) as an ordered queue and
// replayed in order through dispatch on approval.
type pendingAccessReq struct {
	msg          channels.InboundMessage
	slackChannel string

	storedAt time.Time // set by storePendingAccess; drives the TTL sweep
}

// pendingLoginReq is a message parked while its unlinked sender completes the
// sign-in flow. It is replayed through dispatch when the user links, so the
// question they typed is answered without them having to send it again. Parked
// per (user, thread) as an ordered queue: several messages in one thread are
// kept and replayed in order, and messages in other threads are replayed too.
type pendingLoginReq struct {
	msg          channels.InboundMessage
	slackChannel string

	storedAt time.Time // set by parkPendingLogin; drives the TTL sweep
}

// maxParkedPerThread bounds each parked queue (login and access) per (user,
// thread) so a chatty burst does not replay an unbounded string of turns. When
// exceeded the oldest message is dropped, keeping the most recent ones in order.
const maxParkedPerThread = 5

// pendingTTL bounds how long a parked task or access request is kept. Both
// maps hold user content and grow per thread; an entry this old is abandoned
// (the paused A2A task has long been resumable by nobody) and is swept on the
// next store.
const pendingTTL = 24 * time.Hour

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
	switch a.DMMode {
	case "", DMModeServe, DMModeRedirect, DMModeIgnore:
	default:
		return fmt.Errorf("slack: unknown DMMode %q: want %s, %s, or %s",
			a.DMMode, DMModeServe, DMModeRedirect, DMModeIgnore)
	}
	switch a.ChannelMode {
	case "", ChannelModeAll, ChannelModeAllowlist, ChannelModeNone:
	default:
		return fmt.Errorf("slack: unknown ChannelMode %q: want %s, %s, or %s",
			a.ChannelMode, ChannelModeAll, ChannelModeAllowlist, ChannelModeNone)
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
	a.baseCtx = ctx

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
// all, before any access-control or command handling. It drops stale events:
// events whose Slack ts predates this process. Socket Mode redelivers events
// queued while a consumer was disconnected, so without this a gateway restart
// replays — and re-answers — old messages. New messages (ts >= start) always
// pass. Surface gating (DM mode, channel mode) happens in handleInbound, which
// has the context needed to post redirect/notice messages.
func (a *Adapter) acceptEvent(inner slackInnerEvent) bool {
	return !a.staleEvent(inner)
}

// dmMode returns the effective DM handling mode (empty means serve).
func (a *Adapter) dmMode() DMMode {
	if a.DMMode == "" {
		return DMModeServe
	}
	return a.DMMode
}

// channelServed reports whether the bot serves the given (non-DM) channel
// under the configured ChannelMode. Unknown modes are rejected by Start.
func (a *Adapter) channelServed(channel string) bool {
	switch a.ChannelMode {
	case "", ChannelModeAll:
		return true
	case ChannelModeAllowlist:
		return slices.Contains(a.ChannelAllowlist, channel)
	default:
		return false
	}
}

// staleEvent reports whether the event predates this process and DropStaleEvents
// is on. Socket Mode redelivers events queued while a consumer was
// disconnected, so without this a gateway restart replays old events. Uses the
// message ts, falling back to the envelope event_ts (the only timestamp a
// member_joined_channel event carries).
func (a *Adapter) staleEvent(inner slackInnerEvent) bool {
	if !a.DropStaleEvents || a.startUnix == 0 {
		return false
	}
	ts := inner.TS
	if ts == "" {
		ts = inner.EventTS
	}
	if sec := eventUnix(ts); sec > 0 && sec < a.startUnix {
		a.Logger.Info("slack: dropping stale event from before gateway start",
			"event_ts", ts, "channel", inner.Channel)
		return true
	}
	return false
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

// apiClient returns a Slack Web API client using the adapter's bot token and the
// optional test-override base URL. It posts under the app's default identity
// (Swarmgeist); agentClient posts under the agent's identity.
func (a *Adapter) apiClient() *slackAPIClient {
	base := a.APIBase
	if base == "" {
		base = slackAPIBase
	}
	return &slackAPIClient{botToken: a.Secrets.BotToken, baseURL: base}
}

// AgentCardResolver yields an agent's display identity (name/icon) from its A2A
// AgentCard. Implemented by pkg/a2a.AgentCardClient; nil disables card branding.
type AgentCardResolver interface {
	CardIdentity(ctx context.Context, agentRef string) (username, iconURL string)
}

// botID returns this bot's own Slack user ID (via auth.test), caching it. Used
// to recognise the bot's own member_joined_channel event. Returns "" on lookup
// failure (logged); the caller then cannot confirm a self-join and skips the intro.
func (a *Adapter) botID(ctx context.Context) string {
	a.botIDMu.Lock()
	id := a.botUserID
	a.botIDMu.Unlock()
	if id != "" {
		return id
	}
	got, err := a.apiClient().authTest(ctx)
	if err != nil {
		a.Logger.Warn("slack: auth.test failed, cannot resolve bot user ID", "error", err)
		return ""
	}
	a.botIDMu.Lock()
	a.botUserID = got
	a.botIDMu.Unlock()
	return got
}

// postChannelIntro posts the one-time Swarmgeist-branded introduction when the
// bot is added to a channel. Best-effort.
func (a *Adapter) postChannelIntro(ctx context.Context, slackChannel string) {
	if _, err := a.apiClient().postMessage(ctx, slackChannel, channelIntro, ""); err != nil {
		a.Logger.Warn("slack: post channel intro failed", "channel", slackChannel, "error", err)
	}
}

// dmRedirectTTL bounds how often the DM redirect is repeated per IM channel,
// so a user replying to the redirect is not answered with another redirect.
const dmRedirectTTL = time.Hour

// postDMRedirect points a user who DMs the bot to a channel (DMs are not a
// supported surface in channel mode). Posted at most once per IM channel per
// dmRedirectTTL window. Best-effort.
func (a *Adapter) postDMRedirect(ctx context.Context, slackChannel string) {
	now := time.Now()
	a.dmRedirectMu.Lock()
	if a.dmRedirected == nil {
		a.dmRedirected = make(map[string]ttlEntry[struct{}])
	}
	sweepExpired(a.dmRedirected, now)
	if _, seen := a.dmRedirected[slackChannel]; seen {
		a.dmRedirectMu.Unlock()
		return
	}
	a.dmRedirected[slackChannel] = ttlEntry[struct{}]{expires: now.Add(dmRedirectTTL)}
	a.dmRedirectMu.Unlock()

	if _, err := a.apiClient().postMessage(ctx, slackChannel, dmRedirect, ""); err != nil {
		a.Logger.Warn("slack: post DM redirect failed", "channel", slackChannel, "error", err)
	}
}

// postChannelNotServed tells a user who mentioned the bot in an unserved
// channel that the bot is not enabled there. Ephemeral (only the mentioning
// user sees it), at most once per (channel, user) per dmRedirectTTL window.
// Best-effort.
func (a *Adapter) postChannelNotServed(ctx context.Context, slackChannel, userID string) {
	key := slackChannel + "|" + userID
	now := time.Now()
	a.notServedMu.Lock()
	if a.notServedNoticed == nil {
		a.notServedNoticed = make(map[string]ttlEntry[struct{}])
	}
	sweepExpired(a.notServedNoticed, now)
	if _, seen := a.notServedNoticed[key]; seen {
		a.notServedMu.Unlock()
		return
	}
	a.notServedNoticed[key] = ttlEntry[struct{}]{expires: now.Add(dmRedirectTTL)}
	a.notServedMu.Unlock()

	if err := a.apiClient().postEphemeralText(ctx, slackChannel, userID, "", channelNotServed); err != nil {
		a.Logger.Warn("slack: post channel-not-served notice failed", "channel", slackChannel, "error", err)
	}
}

// agentClient returns a Slack client that posts under the agent's AgentCard
// display identity, for the agent's own replies and confirmation prompts.
// Falls back to the app default when no card resolver is set or the lookup is
// empty.
func (a *Adapter) agentClient(ctx context.Context, agentRef string) *slackAPIClient {
	c := a.apiClient()
	if a.AgentCards != nil {
		c.username, c.iconURL = a.AgentCards.CardIdentity(ctx, agentRef)
	}
	return c
}

// agentDisplayName is the agent's human-facing name for Swarmgeist's own
// messages (e.g. the launch announcement): the AgentCard name, or the agentRef
// when no card name is known.
func (a *Adapter) agentDisplayName(ctx context.Context, agentRef string) string {
	if a.AgentCards != nil {
		if name, _ := a.AgentCards.CardIdentity(ctx, agentRef); name != "" {
			return name
		}
	}
	return agentRef
}

// postLaunchAnnouncement posts the Swarmgeist-branded handoff notice when a new
// thread starts, making the app-to-agent transition explicit. Best-effort.
func (a *Adapter) postLaunchAnnouncement(ctx context.Context, slackChannel, threadID, agentRef string) {
	text := fmt.Sprintf("🚀 Bringing in *%s* to help. Keep the conversation in this thread; mention me followed by `/help` to list what I can do.", a.agentDisplayName(ctx, agentRef))
	if _, err := a.apiClient().postMessage(ctx, slackChannel, text, threadID); err != nil {
		a.Logger.Warn("slack: post launch announcement failed", "thread", threadID, "error", err)
	}
}

// signInAnchor is the message coordinates of a posted sign-in prompt, kept so
// a completed link can rewrite the prompt in place (chat.update).
type signInAnchor struct {
	channel string
	ts      string
}

// postSignIn posts the "Sign in to Giant Swarm" prompt for the account-linking
// flow and records its message coordinates so the completed link rewrites it in
// place. It is driven by the explicit /login command and by an unlinked
// user's first turn (which is aborted, not run as the SA). A failure to post is
// logged and swallowed.
func (a *Adapter) postSignIn(ctx context.Context, slackChannel, threadID, slackUser string) {
	url := a.OBO.LinkURL(slackUser)
	if url == "" {
		a.Logger.Warn("slack: empty sign-in link URL, skipping prompt", "user", slackUser)
		a.clearSignInReservation(slackUser, threadID)
		return
	}
	ts, err := a.apiClient().postSignInPrompt(ctx, slackChannel, threadID, url)
	if err != nil {
		a.Logger.Warn("slack: post sign-in prompt failed", "user", slackUser, "error", err)
		a.clearSignInReservation(slackUser, threadID)
		return
	}
	a.recordSignInAnchor(slackUser, threadID, signInAnchor{channel: slackChannel, ts: ts})
	// The post ran outside any lock, so a link callback may have drained the
	// anchors while the prompt was in flight; the just-recorded anchor would
	// then keep a live sign-in button for an already-linked user and suppress
	// re-prompts for the full window. Re-check and converge.
	if _, err := a.OBO.TokenFor(ctx, slackUser); err == nil {
		a.updateSignInAnchors(ctx, slackUser, "", false)
	}
}

// clearSignInReservation removes the (user, thread) throttle entry when no
// prompt message exists for it, so a failed post retries on the next parked
// message instead of suppressing the nudge for the full window. An entry with
// a posted anchor is left alone.
func (a *Adapter) clearSignInReservation(slackUser, threadID string) {
	key := slackUser + "\x00" + threadID
	a.signInPromptedMu.Lock()
	defer a.signInPromptedMu.Unlock()
	if entry, ok := a.signInPrompted[key]; ok && entry.value.ts == "" {
		delete(a.signInPrompted, key)
	}
}

// recordSignInAnchor stores the posted prompt's coordinates under the (user,
// thread) key. It also acts as the nudge-throttle entry, so an explicit /login
// prompt suppresses a redundant parked-message nudge in the same thread.
func (a *Adapter) recordSignInAnchor(slackUser, threadID string, anchor signInAnchor) {
	now := time.Now()
	key := slackUser + "\x00" + threadID
	a.signInPromptedMu.Lock()
	defer a.signInPromptedMu.Unlock()
	if a.signInPrompted == nil {
		a.signInPrompted = make(map[string]ttlEntry[signInAnchor])
	}
	sweepExpired(a.signInPrompted, now)
	a.signInPrompted[key] = ttlEntry[signInAnchor]{value: anchor, expires: now.Add(pendingTTL)}
}

// markConnectorPrompted records a prompt attempt for (user, server) and
// reports whether one is allowed now (outside the cooldown). Check and set
// are atomic so concurrent turns post at most one prompt.
func (a *Adapter) markConnectorPrompted(slackUser, server string) bool {
	now := time.Now()
	a.connectorMu.Lock()
	defer a.connectorMu.Unlock()
	if last, ok := a.connectorPrompted[slackUser][server]; ok && now.Sub(last) < connectorPromptCooldown {
		return false
	}
	if a.connectorPrompted == nil {
		a.connectorPrompted = make(map[string]map[string]time.Time)
	}
	if a.connectorPrompted[slackUser] == nil {
		a.connectorPrompted[slackUser] = make(map[string]time.Time)
	}
	a.connectorPrompted[slackUser][server] = now
	return true
}

// clearConnectorPrompted forgets a prompt attempt (fetch or post failed, the
// user dismissed, or the backend connected) so the next trigger may prompt
// again.
func (a *Adapter) clearConnectorPrompted(slackUser, server string) {
	a.connectorMu.Lock()
	defer a.connectorMu.Unlock()
	delete(a.connectorPrompted[slackUser], server)
}

// takeSignInAnchors returns and clears the user's sign-in prompt entries,
// keeping only anchors that are still addressable (posted successfully and
// unexpired). Draining doubles as the throttle reset, so becoming unlinked
// again (e.g. /logout) prompts anew.
func (a *Adapter) takeSignInAnchors(slackUser string) []signInAnchor {
	prefix := slackUser + "\x00"
	now := time.Now()
	a.signInPromptedMu.Lock()
	defer a.signInPromptedMu.Unlock()
	var anchors []signInAnchor
	for key, entry := range a.signInPrompted {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		delete(a.signInPrompted, key)
		if entry.value.ts != "" && now.Before(entry.expires) {
			anchors = append(anchors, entry.value)
		}
	}
	return anchors
}

// updateSignInAnchors rewrites the user's sign-in prompt messages in place once
// the account link completes, folding the confirmation and the agent handoff
// notice into the message the user is already looking at (the URL button is
// dropped by the rewrite). replaying selects the handoff wording when parked
// messages are about to re-dispatch.
func (a *Adapter) updateSignInAnchors(ctx context.Context, slackUser, email string, replaying bool) {
	anchors := a.takeSignInAnchors(slackUser)
	if len(anchors) == 0 {
		return
	}
	text := "✅ Signed in. I can act on your behalf now."
	if email != "" {
		text = "✅ Signed in as " + email + ". I can act on your behalf now."
	}
	if replaying {
		text += fmt.Sprintf(" Bringing in **%s** to help.", a.agentDisplayName(ctx, a.DefaultAgent))
	}
	client := a.apiClient()
	for _, anchor := range anchors {
		if err := client.chatUpdateMarkdown(ctx, anchor.channel, anchor.ts, text); err != nil {
			a.Logger.Warn("slack: update sign-in prompt after link failed", "user", slackUser, "channel", anchor.channel, "ts", anchor.ts, "error", err)
		}
	}
}

// maybePostSignIn posts the sign-in prompt unless one was already posted for
// this (user, thread) within the current pendingTTL window, so a burst of
// parked messages nudges once instead of once per message. The explicit
// /login command bypasses it (postSignIn directly); a completed link drains
// the window (takeSignInAnchors) so a /logout re-prompts. The entry is
// reserved (with no anchor yet) before posting so concurrent parks nudge once;
// postSignIn overwrites it with the posted message's coordinates.
func (a *Adapter) maybePostSignIn(ctx context.Context, slackChannel, threadID, slackUser string) {
	now := time.Now()
	key := slackUser + "\x00" + threadID
	a.signInPromptedMu.Lock()
	entry, prompted := a.signInPrompted[key]
	if prompted && now.Before(entry.expires) {
		a.signInPromptedMu.Unlock()
		return
	}
	if a.signInPrompted == nil {
		a.signInPrompted = make(map[string]ttlEntry[signInAnchor])
	}
	sweepExpired(a.signInPrompted, now)
	a.signInPrompted[key] = ttlEntry[signInAnchor]{expires: now.Add(pendingTTL)}
	a.signInPromptedMu.Unlock()
	a.postSignIn(ctx, slackChannel, threadID, slackUser)
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

// storePendingAccess appends a newcomer's message to their parked queue for the
// thread while the initiator is asked to approve them. Bounded per (thread,
// user) by maxParkedPerThread (oldest dropped past the cap). Returns true when
// this is the first parked request for the (thread, user), so the caller posts
// the consent prompt once rather than on every parked message (e.g. a burst
// replayed after sign-in).
func (a *Adapter) storePendingAccess(threadID, userID string, req *pendingAccessReq) bool {
	a.pendingAccessMu.Lock()
	defer a.pendingAccessMu.Unlock()
	if a.pendingAccess == nil {
		a.pendingAccess = make(map[string]map[string][]*pendingAccessReq)
	}
	byUser := a.pendingAccess[threadID]
	if byUser == nil {
		byUser = make(map[string][]*pendingAccessReq)
		a.pendingAccess[threadID] = byUser
	}
	existed := len(byUser[userID]) > 0
	req.storedAt = time.Now()
	queue := append(byUser[userID], req)
	if len(queue) > maxParkedPerThread {
		queue = queue[len(queue)-maxParkedPerThread:]
	}
	byUser[userID] = queue
	for thread, users := range a.pendingAccess {
		for user, q := range users {
			kept := q[:0]
			for _, r := range q {
				if time.Since(r.storedAt) <= pendingTTL {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				delete(users, user)
			} else {
				users[user] = kept
			}
		}
		if len(users) == 0 {
			delete(a.pendingAccess, thread)
		}
	}
	return !existed
}

// takePendingAccess atomically retrieves and removes a user's parked messages
// for the thread, in arrival order. Entries past pendingTTL are dropped.
func (a *Adapter) takePendingAccess(threadID, userID string) []*pendingAccessReq {
	a.pendingAccessMu.Lock()
	defer a.pendingAccessMu.Unlock()
	byUser := a.pendingAccess[threadID]
	if byUser == nil {
		return nil
	}
	queue := byUser[userID]
	delete(byUser, userID)
	if len(byUser) == 0 {
		delete(a.pendingAccess, threadID)
	}
	fresh := queue[:0]
	for _, r := range queue {
		if time.Since(r.storedAt) <= pendingTTL {
			fresh = append(fresh, r)
		}
	}
	return fresh
}

// parkPendingLogin appends a message to the user's parked queue for its thread
// so it can be replayed once the link completes. Bounded per thread by
// maxParkedPerThread (oldest dropped past the cap). Abandoned entries are
// swept opportunistically.
func (a *Adapter) parkPendingLogin(slackUser string, req *pendingLoginReq) {
	a.pendingLoginMu.Lock()
	defer a.pendingLoginMu.Unlock()
	if a.pendingLogin == nil {
		a.pendingLogin = make(map[string]map[string][]*pendingLoginReq)
	}
	byThread := a.pendingLogin[slackUser]
	if byThread == nil {
		byThread = make(map[string][]*pendingLoginReq)
		a.pendingLogin[slackUser] = byThread
	}
	req.storedAt = time.Now()
	queue := append(byThread[req.msg.ThreadID], req)
	if len(queue) > maxParkedPerThread {
		queue = queue[len(queue)-maxParkedPerThread:]
	}
	byThread[req.msg.ThreadID] = queue
	for user, threads := range a.pendingLogin {
		for thread, q := range threads {
			kept := q[:0]
			for _, r := range q {
				if time.Since(r.storedAt) <= pendingTTL {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				delete(threads, thread)
			} else {
				threads[thread] = kept
			}
		}
		if len(threads) == 0 {
			delete(a.pendingLogin, user)
		}
	}
}

// parkForLogin parks msg for replay after sign-in and posts the (throttled)
// sign-in prompt. The TokenFor miss that brought the caller here and the park
// are not atomic: a link callback firing in between would find nothing to
// drain and leave the parked message stranded until the TTL sweep. Re-checking
// after the park closes that window; when the user turns out to be linked the
// queue is drained immediately through the normal replay path instead of
// prompting.
func (a *Adapter) parkForLogin(ctx context.Context, msg channels.InboundMessage, slackChannel, slackUser string) {
	a.parkPendingLogin(slackUser, &pendingLoginReq{msg: msg, slackChannel: slackChannel})
	if _, err := a.OBO.TokenFor(ctx, slackUser); err == nil {
		a.OnUserLinked(ctx, slackUser, "")
		return
	}
	a.maybePostSignIn(ctx, slackChannel, msg.ThreadID, slackUser)
}

// takePendingLogin atomically retrieves and removes a user's parked messages,
// grouped by thread and kept in arrival order within each thread. Entries past
// pendingTTL are dropped, so an abandoned sign-in completed a day later does
// not replay stale questions.
func (a *Adapter) takePendingLogin(slackUser string) map[string][]*pendingLoginReq {
	a.pendingLoginMu.Lock()
	defer a.pendingLoginMu.Unlock()
	byThread := a.pendingLogin[slackUser]
	delete(a.pendingLogin, slackUser)
	for thread, queue := range byThread {
		kept := queue[:0]
		for _, r := range queue {
			if time.Since(r.storedAt) <= pendingTTL {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			delete(byThread, thread)
		} else {
			byThread[thread] = kept
		}
	}
	if len(byThread) == 0 {
		return nil
	}
	return byThread
}

// OnUserLinked rewrites the sign-in prompt message(s) into a confirmation and
// replays the messages the user sent before signing in, once their muster
// identity is linked. Registered as the musterlink OnLinked hook, whose contract
// is that it must not block: HandleCallback renders the "signed in, close this
// tab" page only after it returns. So both the prompt rewrite and the replay
// are dispatched on their own goroutines and this returns promptly. Each thread's
// queue is replayed in order on its own goroutine (dispatch serializes per
// thread, so ordering only holds within a sequential drain); threads replay
// concurrently.
//
// The background work runs on the adapter lifecycle context, not the OAuth
// callback context: the callback context is request-scoped, so a shutdown would
// not cancel work dispatched from it, whereas normal dispatch (on the lifecycle
// context) is cancelled. Falls back to the passed context when the adapter was
// constructed without Start (direct-construction tests).
func (a *Adapter) OnUserLinked(ctx context.Context, slackUser, email string) {
	bgCtx := a.baseCtx
	if bgCtx == nil {
		bgCtx = ctx
	}
	queues := a.takePendingLogin(slackUser)
	// The prompt rewrite announces the agent handoff only when a replay will
	// actually reach the agent: bare auth utterances are satisfied by the link
	// itself, and a newcomer's replay lands at the initiator's consent prompt
	// instead, so both keep the plain confirmation.
	replaying := false
	for threadID, queue := range queues {
		for _, req := range queue {
			if !isBareAuthUtterance(req.msg.Text) && a.accessPolicy().Allowed(threadID, slackUser) {
				replaying = true
			}
		}
	}
	// updateSignInAnchors does blocking chat.update round-trips; running them
	// inline would delay the sign-in success page. Ordering against replay is
	// immaterial: the prompt and the agent's thread reply are independent
	// messages.
	go a.updateSignInAnchors(bgCtx, slackUser, email, replaying)
	for _, queue := range queues {
		go func() {
			for _, req := range queue {
				// A bare "login"-style message asked for the sign-in that just
				// completed; replaying it would send a stale request to the agent.
				if isBareAuthUtterance(req.msg.Text) {
					a.Logger.Debug("slack: dropping parked sign-in request, link satisfied it",
						"user", slackUser, "thread", req.msg.ThreadID)
					continue
				}
				if err := a.replayDispatch(bgCtx, req.msg, req.slackChannel); err != nil && !errors.Is(err, context.Canceled) {
					a.Logger.Error("slack: replay after sign-in failed", "user", slackUser, "thread", req.msg.ThreadID, "error", err)
				}
			}
		}()
	}
}

// authUtterances are messages that are nothing but a request to sign in. A
// parked one is satisfied by the link completing, so it is dropped at replay
// instead of confusing the agent with a stale "login".
var authUtterances = map[string]struct{}{
	cmdLogin:       {},
	"log in":       {},
	"sign in":      {},
	"signin":       {},
	"connect":      {},
	"/" + cmdLogin: {},
}

// isBareAuthUtterance reports whether text, mention-stripped and normalized,
// exactly matches an auth utterance (optionally with trailing punctuation).
// Never a substring match: "login to grafana is broken" must replay.
func isBareAuthUtterance(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(StripMention(text)))
	normalized = strings.TrimSpace(strings.TrimRight(normalized, ".!?"))
	_, ok := authUtterances[normalized]
	return ok
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
// dedup, member-join intro, accept-gate, normalise, active-thread gate (for
// channel thread replies), command handling, then dispatch. Both transports
// (the Events API HTTP handler and the Socket Mode reader) call it so the two
// behave identically, including deduplication. ctx is the adapter-lifecycle
// context; eventID is the delivery's Slack event_id ("" when the transport
// carries none).
func (a *Adapter) handleInbound(ctx context.Context, inner slackInnerEvent, eventID string) {
	if eventID != "" && a.seenEvent(eventID) {
		a.Logger.Info("slack: dropping duplicate event delivery", "event_id", eventID)
		return
	}
	// The bot being added to a channel -> one-time intro, only where the
	// channel is actually served. Only the bot's own join (user == bot ID)
	// triggers the intro.
	if inner.Type == evtMemberJoined {
		if a.staleEvent(inner) {
			return
		}
		if inner.User != "" && inner.User == a.botID(ctx) && a.channelServed(inner.Channel) {
			a.postChannelIntro(ctx, inner.Channel)
		}
		return
	}
	if !a.acceptEvent(inner) {
		return
	}
	if inner.isDM() {
		switch a.dmMode() {
		case DMModeRedirect:
			// Bot messages and non-message subtypes are skipped (no reply loop).
			if inner.BotID == "" && inner.SubType == "" {
				a.postDMRedirect(ctx, inner.Channel)
			}
			return
		case DMModeIgnore:
			return
		}
	} else if !a.channelServed(inner.Channel) {
		// A deliberate mention in an unserved channel gets one ephemeral
		// notice so the silence does not read as an outage; everything else
		// is dropped.
		if inner.Type == evtAppMention && inner.BotID == "" && inner.User != "" {
			a.postChannelNotServed(ctx, inner.Channel, inner.User)
		}
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
	a.seedInitiatorFromRoot(ctx, inner.Channel, msg.ThreadID, msg.MessageID)
	if cmd := parseCommand(msg.Text); cmd != nil {
		if a.handleCommand(ctx, cmd, msg.Subject, inner.Channel, msg.ThreadID) {
			return
		}
	}
	if err := a.dispatch(ctx, msg, inner.Channel); err != nil {
		switch {
		case errors.Is(err, errThreadBusy):
			if _, perr := a.apiClient().postMessage(ctx, inner.Channel, busyNotice, msg.ThreadID); perr != nil {
				a.Logger.Warn("slack: post busy notice failed", "thread", msg.ThreadID, "error", perr)
			}
		case !errors.Is(err, context.Canceled):
			a.Logger.Error("slack: dispatch error", "channel", inner.Channel, "error", err)
		}
	}
}

// errThreadBusy reports a turn rejected because another turn holds the
// thread's slot, so the caller chooses between notifying the user and
// requeueing the message.
var errThreadBusy = errors.New("slack: thread busy")

// replayDispatch delivers a previously parked message through dispatch. The
// parked user never knowingly raced anyone, so a held thread slot does not
// reject the message with the busy notice: the replay waits for the slot and
// retries, including after losing the re-acquire race to a concurrently
// arriving turn. Blocking (a replayed turn runs to completion inside the
// dispatch call), which lets a caller drain a queue in order; run it off any
// latency-sensitive goroutine.
func (a *Adapter) replayDispatch(ctx context.Context, msg channels.InboundMessage, slackChannel string) error {
	for {
		err := a.dispatch(ctx, msg, slackChannel)
		if !errors.Is(err, errThreadBusy) {
			return err
		}
		idle := make(chan struct{})
		a.whenThreadIdle(msg.ThreadID, func() { close(idle) })
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// rootAuthorLookupTimeout bounds the conversations.replies call that seeds a
// thread's initiator, so a slow Slack API cannot stall inbound handling.
const rootAuthorLookupTimeout = 3 * time.Second

// seedInitiatorFromRoot seeds a thread's initiator from its first human author.
// The access policy is process-local, so after a restart the first poster into
// a pre-existing thread would otherwise take it over as initiator. Only a reply
// into a thread with no recorded initiator triggers the lookup; when no human
// author can be determined (fetch failure, all-bot prefix) the first-poster
// behavior stands.
//
// The reseed is restart recovery, and faithful only for bot-rooted threads
// (top-level mention or DM) where the root author is the original initiator;
// for a thread the bot was invited into as a reply the root author never
// summoned the bot, so the reseed is best-effort there.
//
// A thread with no recorded initiator has three causes: a restart (state lost),
// a threadAccessTTL sweep (state deliberately expired), or a thread never
// engaged before. Within threadAccessTTL of process start nothing can yet have
// been swept, so a missing initiator is pre-restart state loss and restoring
// the root author is safe. Past that window it is a swept or never-seen thread;
// in both the summoner of the fresh mention should own the thread, so the reseed
// is suppressed and dispatch's first-interactor rule stands. Reseeding past the
// window would resurrect state the TTL cleared and gate the user whose mention
// re-engaged the thread.
func (a *Adapter) seedInitiatorFromRoot(ctx context.Context, slackChannel, threadID, messageID string) {
	if threadID == "" || threadID == messageID {
		return
	}
	// A 1:1 DM has a single human, so there is no other participant to evict:
	// first-poster-becomes-initiator is always correct, and the reseed would
	// only cost a lookup.
	if isDMChannelID(slackChannel) {
		return
	}
	if a.startUnix == 0 || time.Now().Unix()-a.startUnix >= int64(threadAccessTTL.Seconds()) {
		return
	}
	if a.accessPolicy().Initiator(threadID) != "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, rootAuthorLookupTimeout)
	defer cancel()
	author, err := a.apiClient().threadInitiator(ctx, slackChannel, threadID)
	if err != nil || author == "" {
		a.Logger.Debug("slack: thread initiator unavailable, first poster becomes initiator",
			"thread", threadID, "error", err)
		return
	}
	a.accessPolicy().SetInitiator(threadID, author)
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
			if _, err := a.OBO.TokenFor(ctx, slackUser); err != nil {
				if errors.Is(err, musterlink.ErrNotLinked) {
					a.Logger.Info("slack: newcomer not linked, prompting sign-in", "user", slackUser)
					// Hold the message so signing in resumes it (the replay re-enters
					// dispatch, now linked, and falls through to the access prompt).
					a.parkForLogin(ctx, msg, slackChannel, slackUser)
					return nil
				}
				// A transient mint failure surfaces to the newcomer now; parking the
				// message would defer it to a confusing error after the initiator's
				// consent.
				a.Logger.Warn("slack: newcomer token unavailable, not parking", "user", slackUser, "error", err)
				if perr := a.apiClient().postEphemeralText(ctx, slackChannel, slackUser, msg.ThreadID, tokenErrorNotice); perr != nil {
					a.Logger.Warn("slack: post token-error message failed", "user", slackUser, "error", perr)
				}
				return nil
			}
		}
		if a.storePendingAccess(msg.ThreadID, slackUser, &pendingAccessReq{msg: msg, slackChannel: slackChannel}) {
			a.postAccessPrompt(ctx, slackChannel, msg.ThreadID, initiator, slackUser)
		}
		return nil
	}

	msg.AgentRef = a.DefaultAgent

	// Serialize turns per thread: a thread maps to one kagent session, and
	// concurrent turns on one session interleave its event log into incoherent
	// history. Reject a turn that arrives while another is in flight rather than
	// queueing it, so a stale follow-up is not answered minutes late. Acquire
	// before taking the pending task so a rejected turn leaves it for later.
	if !a.acquireThread(msg.ThreadID) {
		return errThreadBusy
	}
	defer a.releaseThread(msg.ThreadID)

	a.resolveSubjectEmail(ctx, &msg)

	// A turn must carry the sending user's human token, never the gateway's
	// machine identity. Resolve it BEFORE consuming any pending task so an abort
	// leaves the pending TaskID intact and the reply stays retryable.
	token, ok, signIn := a.humanToken(ctx, slackChannel, msg.ThreadID, slackUser)
	if signIn {
		// Hold the message so it is answered after the user signs in, rather than
		// dropped and re-typed. resolveSubjectEmail above rewrote msg.Subject to the
		// email; restore the raw Slack user ID so the replay keys access control the
		// same way this turn did. An immediate drain (already-linked race) replays
		// once this dispatch releases the thread slot.
		parked := msg
		parked.Subject = slackUser
		a.parkForLogin(ctx, parked, slackChannel, slackUser)
	}
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

	// New channel thread (a root mention starting a conversation): post the
	// Swarmgeist handoff notice before the agent takes over, so the app-to-agent
	// transition is explicit. Posted only once the agent resolved, so a resolve
	// failure does not announce a launch and then error out. Skipped for thread
	// replies, resumed tasks, and DMs (a 1:1 DM is the agent conversation
	// itself, with no channel handoff). Slack DM channel IDs start with "D".
	if firstSight && msg.ThreadID == msg.MessageID && msg.TaskID == "" && !strings.HasPrefix(slackChannel, "D") {
		a.postLaunchAnnouncement(ctx, slackChannel, msg.ThreadID, msg.AgentRef)
	}

	turnCtx, done := a.registerTurn(ctx, msg.ThreadID)
	defer done()

	a.logTurnDispatch(msg, slackUser, task != nil)

	deltas, err := a.gw.SendCompletion(turnCtx, ref, msg)
	if err != nil {
		restoreTask()
		return fmt.Errorf("slack: send completion: %w", err)
	}

	// The turn context feeds the whole stream so /stop cancels the turn, and an
	// aborted consumer releases the producer goroutine.
	var carried channels.TurnUsage
	if task != nil {
		carried = task.Usage
	}
	err = a.streamResponse(turnCtx, a.agentClient(ctx, msg.AgentRef), deltas, msg, slackUser, slackChannel, msg.ThreadID, msg.MessageID, thinkingPlaceholder, carried)
	if isCorruptSessionErr(err) {
		a.recoverCorruptSession(ctx, msg, slackChannel)
	}
	return err
}

// isCorruptSessionErr reports whether a turn failed because the session's
// persisted history is invalid: an interrupted earlier turn left a tool call
// with no result, and the model API rejects the whole history, so every later
// turn on the session fails identically. The failure arrives as an opaque A2A
// error string, so it is matched on the model API's wording; the
// invalid_request_error class is required so an error that merely quotes
// agent output mentioning tool_use/tool_result cannot trigger the recovery
// (which irreversibly deletes the session).
//
// The match is coupled to the model API's error envelope: if the upstream ever
// reshapes it so this wording is absent, recovery stops and the thread fails
// silently forever again. A typed signal from the A2A layer would remove the
// coupling; until then TestCorruptSession_ResetAndNotice pins the live shape.
func isCorruptSessionErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "invalid_request_error") &&
		strings.Contains(s, "tool_use") && strings.Contains(s, "tool_result")
}

// sessionResetter is the optional Gateway capability that deletes a thread's
// kagent session. The Facade implements it; without it recovery degrades to
// advising a new thread.
type sessionResetter interface {
	ResetSession(ctx context.Context, msg channels.InboundMessage) (bool, error)
}

// recoverCorruptSession deletes the thread's kagent session after a
// corrupt-history failure so the next message starts a fresh session instead
// of failing forever, and tells the user what happened. Best-effort: when the
// reset is unavailable or fails, the notice advises starting a new thread.
func (a *Adapter) recoverCorruptSession(ctx context.Context, msg channels.InboundMessage, slackChannel string) {
	reset := false
	if sr, ok := a.gw.(sessionResetter); ok {
		ok, err := sr.ResetSession(ctx, msg)
		if err != nil {
			a.Logger.Warn("slack: corrupt-session reset failed", "thread", msg.ThreadID, "error", err)
		}
		reset = ok && err == nil
	}
	a.Logger.Info("slack: corrupt session detected",
		"record", "session_corrupt",
		"thread", msg.ThreadID,
		"channel_id", msg.ChannelID,
		"reset", reset)
	note := corruptSessionResetNotice
	if !reset {
		note = corruptSessionStuckNotice
	}
	if _, err := a.apiClient().postMessage(ctx, slackChannel, note, msg.ThreadID); err != nil {
		a.Logger.Warn("slack: post corrupt-session notice failed", "thread", msg.ThreadID, "error", err)
	}
}

// linkedIdentitySource is the optional OBOTokenSource extension the dispatch
// record uses to attach the muster subject. *musterlink.Linker satisfies it;
// sources without it just log an empty sub.
type linkedIdentitySource interface {
	LinkedIdentity(slackUserID string) (sub, email string, ok bool)
}

// logTurnDispatch emits the per-turn dispatch record: which agent this turn
// invokes for which Slack user under which muster identity. It is the
// gateway-side anchor for joining a turn to muster's per-call log. The muster
// session ID is not derivable client-side from the forwarded token, so the
// join key is (sub, thread_id, task_id, timestamp).
func (a *Adapter) logTurnDispatch(msg channels.InboundMessage, slackUser string, resume bool) {
	var sub string
	if ident, ok := a.OBO.(linkedIdentitySource); ok {
		sub, _, _ = ident.LinkedIdentity(slackUser)
	}
	a.Logger.Info("slack: dispatching turn",
		"record", "turn_dispatch",
		"agent", msg.AgentRef,
		"slack_user", slackUser,
		"subject", msg.Subject,
		"sub", sub,
		"channel_id", msg.ChannelID,
		"thread_id", msg.ThreadID,
		"message_id", msg.MessageID,
		"task_id", msg.TaskID,
		"resume", resume)
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
// Shared by message dispatch and the button-click resume path, so an approved
// tool call always runs under the approver's identity. Returns ok=false when
// the turn must not run. When OBO is disabled the token is empty and ok is
// true.
// The caller drives the sign-in prompt on signIn=true so the message path can
// first park the turn for replay after linking, while the button-resume path
// (which has no message to replay) just prompts. A transient failure is surfaced
// here and returns ok=false, signIn=false.
func (a *Adapter) humanToken(ctx context.Context, slackChannel, threadID, slackUser string) (token string, ok, signIn bool) {
	if a.OBO == nil {
		return "", true, false
	}
	if slackUser == "" {
		a.Logger.Warn("slack: aborting turn without a slack user (no human token possible)")
		return "", false, false
	}
	token, err := a.OBO.TokenFor(ctx, slackUser)
	switch {
	case err == nil:
		return token, true, false
	case errors.Is(err, musterlink.ErrNotLinked):
		a.Logger.Info("slack: link unavailable (unlinked or refresh token dead), prompting sign-in", "user", slackUser)
		return "", false, true
	default:
		a.Logger.Warn("slack: human token unavailable, aborting turn", "user", slackUser, "error", err)
		if perr := a.apiClient().postEphemeralText(ctx, slackChannel, slackUser, threadID, tokenErrorNotice); perr != nil {
			a.Logger.Warn("slack: post token-error message failed", "user", slackUser, "error", perr)
		}
		return "", false, false
	}
}

// maxTurnDuration bounds a single turn's stream. The A2A hop has no HTTP
// client timeout (one would sever long turns mid-stream and corrupt the agent
// session), so this is the backstop that eventually frees the thread slot when
// the upstream wedges without closing the connection.
const maxTurnDuration = 30 * time.Minute

// registerTurn installs a cancelable in-flight turn for threadID so /stop can
// cancel it, and returns the turn context plus a cleanup func that cancels the
// turn and removes only this turn's registry entry (even if a later turn on the
// same thread has already replaced it).
func (a *Adapter) registerTurn(ctx context.Context, threadID string) (context.Context, func()) {
	turnCtx, cancel := context.WithTimeout(ctx, maxTurnDuration)
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

// streamResponse renders turn progress (reactions on triggerTS, or a text
// placeholder), streams deltas into a Block Kit markdown reply, and, when the
// agent pauses for input, registers the pending task and posts the HITL prompt.
// Shared by dispatch (a new turn; triggerTS is the user message) and
// handleDecision (a button-click resume; empty triggerTS uses text progress).
// ctx is the turn context (/stop cancels it); carried seeds the usage counters
// when the turn resumes a paused one so /usage reports the whole turn.
// slackUser is the RAW Slack user ID (never the resolved email in msg.Subject),
// so the ephemeral connector prompt reaches a valid chat.postEphemeral user.
func (a *Adapter) streamResponse(ctx context.Context, client *slackAPIClient, deltas <-chan channels.OutboundDelta, msg channels.InboundMessage, slackUser, slackChannel, threadID, triggerTS, placeholder string, carried channels.TurnUsage) error {
	// replyTS is the message the streamed answer edits: the text-mode placeholder,
	// or "" in reactions mode (the writer posts the first answer message lazily).
	prog, replyTS := a.startProgress(ctx, client, slackChannel, threadID, triggerTS, placeholder)

	w := newBatchedWriterWithClient(client, slackChannel, replyTS, threadID, a.detailsLevel(threadID), a.Logger)
	w.turnUsage = carried
	w.adapter = a
	w.slackUser = slackUser
	w.connectorPrompts = a.ConnectorPrompts

	// cleanupCtx survives the turn context so a /stop-cancelled turn still gets
	// its progress indicator cleared and terminal notes posted.
	cleanupCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	}

	// Drive the turn. A DeltaPrompt pauses the stream waiting on the user: the
	// turn is not over, so its usage so far travels with the pending task and is
	// recorded only when the turn actually ends.
	if err := w.run(ctx, deltas); err != nil {
		a.recordTurnUsage(threadID, slackChannel, w.turnUsage)
		cctx, cancel := cleanupCtx()
		defer cancel()
		// A cancelled turn context means the stop was intentional (/stop, shutdown):
		// clear the working indicator silently instead of signalling a failure. A
		// deadline expiry (maxTurnDuration backstop) is a failure, not a stop, and
		// falls through to the failure signalling below.
		if errors.Is(ctx.Err(), context.Canceled) {
			prog.clear(cctx)
			return err
		}
		prog.failed(cctx)
		// In text mode prog.failed is a no-op (no reaction to swap), so the
		// placeholder would linger as "thinking" with no failure signal.
		// Replace it with a terminal note, as the empty-output path does —
		// unless the placeholder already carries streamed answer text, in
		// which case the note posts as a new message so the delivered
		// content survives.
		if prog.reactTS == "" {
			noteTS := replyTS
			if w.wroteContent() {
				noteTS = ""
			}
			a.postTerminalNote(cctx, client, slackChannel, threadID, noteTS, failedNote)
		}
		return err
	}

	if pd := w.promptDelta; pd != nil {
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
	a.recordTurnUsage(threadID, slackChannel, w.turnUsage)
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
	// EventTS is the event envelope timestamp; the only timestamp a
	// member_joined_channel event carries.
	EventTS string `json:"event_ts,omitempty"`
}

// isDM reports whether the event originated in a 1:1 direct message. Slack sets
// channel_type "im" for DMs and uses a "D…" channel ID; either is sufficient.
func (e slackInnerEvent) isDM() bool {
	return e.ChannelType == "im" || isDMChannelID(e.Channel)
}

// isDMChannelID reports whether a Slack channel ID names a 1:1 DM
// conversation ("D…"). Used where only the channel ID is available (no event
// carrying channel_type).
func isDMChannelID(channelID string) bool {
	return strings.HasPrefix(channelID, "D")
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
