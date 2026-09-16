# Slack channel adapter

The Slack adapter lets workspace members talk to Klaus by mentioning the bot or sending it
a direct message. It is disabled by default and must be enabled explicitly.

Two connection modes are supported:

| Mode         | Value        | When to use                                           |
|--------------|-------------|-------------------------------------------------------|
| Events API   | `events`    | Production. Requires a public HTTPS webhook URL.      |
| Socket Mode  | `socketmode`| Development. No public URL required.                  |

## How it fits together: surfaces, sessions, sign-in

Three separate concepts decide where a message appears, which agent session it
lands in, and whether the gateway will act as the user. They are easy to
conflate, so they are described here in one place; the operational sections
below assume this model.

### Surfaces

DMs and channels are two independent surfaces, gated separately:

- `SLACK_DM_MODE` gates the DM surface (`message.im`, in the `D…` channel):
  `serve` (answer, the default), `redirect` (point the user at channels), or
  `ignore` (drop silently).
- `SLACK_CHANNEL_MODE` gates channels: `all` (every channel the bot is invited
  to, the default), `allowlist` (only the IDs in `SLACK_CHANNEL_ALLOWLIST`), or
  `none` (DM-only deployments).

For an **Agent-type Slack app** the DM surface *is* the assistant pane: Slack
replaces the top-level DM composer, so every user message arrives threaded
(`thread_ts` is always set) and plain top-level DMs do not occur. The
per-message-thread DM path (and the channel `/usage` fallback that goes with it)
applies only to non-Agent deployments.

While a turn runs, the thread carries Slack's **native working indicator**. It
is driven by the agent session's lifecycle status
(`agents.sessions.setStatus`, granular bot token with `chat:write`), which the
adapter sets to `processing` when the turn starts and back to `active` on exit:
normal end, stream error, `/stop`. A turn that pauses on a HITL prompt — an
approval, an `ask_user` question, a form — ends in `suspended` instead, which
Slack renders as *waiting for you*, so a conversation that needs an answer is
told apart from a finished one at a glance. The user's answer starts the next
turn, which sets `processing` again. Nothing leaves `suspended` on its own, so
the two moments a prompt dies do it explicitly: the 24-hour sweep of pending
tasks releases the sessions of the prompts it drops, and a click on a prompt
the gateway no longer holds (a restart loses them all) releases that thread's
session as it replaces the buttons with `_Already answered._`.
The explicit idle state is mandatory — unlike the legacy assistant status, this
one does **not** clear itself when the app posts, and a session left in
`processing` keeps spinning for up to an hour. Both surfaces get it: DM threads
and channel threads alike, because `channel_id` and `thread_ts` are always sent
(the bot must be a member of the channel).

The indicator carries no text of its own, so the live tool ticker
(`⏳ tool… · step N`) stays a message on every surface: it is what says *what*
the agent is working on, and it collapses into the receipt (`🛠️ N steps · …`)
when a segment closes or the turn ends. Installs where the method is
unavailable — the scope missing from the bot token, the wrong token type, or
agent messaging disabled for the workspace — drop the native indicator for the
rest of the process lifetime and keep the message ticker. A `not_authorized`
rejection (the bot is not a member of that one channel) only costs that call.

The indicator also carries Slack's **native stop button**, but only for an app
subscribed to the `agent_session_stopped` bot event — the subscription is what
draws the button, so an existing Slack app must have it added by hand (Event
Subscriptions → Subscribe to bot events) before users see it. Pressing it is
equivalent to `/stop`: the adapter cancels the thread's in-flight turn and
confirms with `⏹ Stopped by @presser.` in the thread — the notice names the
presser because, unlike a typed `/stop`, the press leaves no message of its own,
so it is the thread's only record of who stopped the turn. It carries the same
per-thread access rule as `/stop` — only the thread owner and the people they
allowed can interrupt the agent — and refuses anyone else ephemerally, since a
press nobody saw being made should not get an answer the whole thread reads.
Slack does not move the session out of `processing` by itself, so a press that
finds nothing running (a stranded indicator, or one racing the turn's last exit)
sets `active` directly instead of posting anything. A thread waiting on an
approval prompt is left exactly as it is: the button cannot normally reach one,
and in the race where it does, the prompt is still on screen and the user
answers it or types `/stop`.

The `processing` call of a conversation's first turn also **names the
session** (`agents.sessions.setStatus` takes a `title`), so the Messages tab
timeline lists conversations as "Investigate CPU alert on gazelle" rather than
untitled. The title is the conversation's opening message, normalised: the bot
mention, an `/agent "<name>"` selector and any other leading slash verb are
stripped (they address the bot, they do not describe the conversation),
whitespace collapses to single spaces, and the result is cut at a word boundary
to Slack's 200-character limit with a trailing `…`. It is sent only with the
turn that opens the conversation — a channel mention rooting its own thread, the
first message of a new assistant-pane chat (which is never its own thread root:
the pane's thread anchor is Slack's), or the slash command's question — because
Slack applies a title when the call *creates* the session and ignores it
afterwards, which is also what keeps a title a user renamed by hand from being
overwritten. The title is derived when the message is dispatched and sent by the
turn that eventually runs, so an opening message held for sign-in still names
the session once it replays. A first message that normalises to nothing (a bare
command, an upload with no caption) sends no title and leaves Slack to name the
session. Should Slack refuse the titled call, the status is sent again without
the title, so a refused title never costs the turn its indicator.

### Threads and sessions

- `threadID` is `thread_ts` if set, otherwise the message `ts`.
- The A2A `contextID` is a hash of `(channel, channelID, "", threadID,
  agentRef)`. The user slot is deliberately empty: a thread is **thread-scoped**,
  so every participant in it shares one `contextID` and therefore one kagent
  session.
- One assistant thread (or one channel thread) maps to one stable `contextID`
  for its whole life. A "New chat" in the assistant pane is a new thread, and
  therefore a new session, by design.
- kagent looks sessions up by `(contextID, user_id)`, where `user_id` derives
  from the forwarded token subject. Changing the identity configuration (the
  subject claim, or the Dex connector) changes `user_id` and orphans every
  existing session. kagent sessions have no TTL, so orphaned sessions persist.
- Because `user_id` follows the forwarded token, the whole thread runs under the
  **initiator's** identity: a granted collaborator's turn forwards the
  initiator's token, not the collaborator's, so history and tool calls stay in
  the one shared session rather than forking per sender. The collaborator's real
  identity is attached to the message as attribution, so the agent still sees who
  spoke. If the initiator's token cannot be minted (they are unlinked), the turn
  falls back to the sender's own identity rather than the gateway service
  account. kagent v0.9.9 has no per-caller identity within a session; when that
  lands (kagent#1933, #2181) each caller's own identity replaces this.
- Each thread's durable state — its agent, its initiator, and the collaborators the
  initiator allowed — lives in a thread record in the routing store, at the thread's plain
  key (`slack|<channelID>||<threadID>`, the user and agent slots empty) next to its
  AgentInstance binding (the same key with the agent ref appended). It is the only carrier:
  the gateway never reads Slack history to recover any of it. The record slides a 30-day TTL
  on every handled message; the AgentInstance binding beside it never expires. A 24-hour
  access window — who may instruct the agent without the initiator's approval — is computed
  from the record's last-seen time: after 24 hours of silence the initiator and any grants are
  void and the next person to mention the bot becomes the new initiator, while the agent
  binding itself is untouched. A thread whose record has expired after 30 idle days but whose
  AgentInstance binding still exists keeps running on that bound agent — the gateway looks the
  binding up directly instead of starting over.

### Two auth layers

These are independent; a user can have completed one and not the other.

1. **The gateway's account link** (`/login`, musterlink): binds a Slack user to
   a Giant Swarm identity. The callback enforces an email match between the
   OAuth identity and the Slack profile email. GitHub-backed sign-in releases
   the GitHub *primary* email by default, which is the usual mismatch cause;
   dex deployments can set `preferredEmailDomain` on the GitHub connector
   (supported since dex v2.36.0) so a verified email on the work domain wins
   over the primary. This is what the "Sign in to Giant Swarm" prompt starts.
2. **The agent's own per-backend connector OAuth**, brokered by muster
   (`core_auth_login`): authorizes the agent to call a specific backend as the
   user. It is separate from layer 1 and is triggered by the agent, not the
   sign-in prompt.

## Slack app setup

Use `deploy/slack/manifest.yaml` to create and configure the Slack app in one step:

1. Go to [api.slack.com/apps](https://api.slack.com/apps) → **Create New App** → **From an
   app manifest**.
2. Paste the contents of `deploy/slack/manifest.yaml` and follow the wizard.
3. After creation, collect the following credentials from the Slack console:
   - **Signing Secret** (Settings → Basic Information)
   - **Bot Token** (OAuth & Permissions → Bot User OAuth Token)
   - **App-Level Token** (Settings → Basic Information → App-Level Tokens) — Socket Mode only

The app subscribes to two bot events:

- `app_mention` — fires when a user `@`-mentions the bot in any channel
- `message.im` — fires for direct messages to the bot

The manifest also declares a slash command (`/swarmgeist` by default; the name is per app and the
gateway does not depend on it) and the `commands` scope it needs. Manifest changes are applied by
hand at api.slack.com/apps; adding the `commands` scope to an install that lacks it requires a
reinstall.

## Agent routing

Every Slack thread is routed to a single agent via the A2A executor. A conversation picks its
agent when it opens, through one of two entry points, and keeps it for life:

- **Mention with a prefix**: `@bot /agent "<display name>" <question>` or
  `@bot /agent <technical-name> <question>` starts a conversation in any thread with no agent
  recorded yet — a root `@`-mention, or a reply inside an existing thread that has none of its
  own (an alert another app posted, say). The technical name may carry the served
  namespace (`kagent/sre-agent`); it names the same agent as the bare name. Without a prefix the conversation goes to the default
  agent (`slack.defaultAgent`). Inside a thread that already has a conversation, naming its own
  agent again is a no-op — the turn dispatches as a normal reply — and naming a different agent
  is refused: the session's identity is tied to the first agent, and a mid-conversation switch
  would silently start an empty one.
- **Slash command**: `/swarmgeist [question]` in a channel opens a modal with an agent select over
  the live roster (the default agent preselected) and a question box. On submit the gateway posts
  the conversation root itself, under the agent's identity ("💬 @user asked *Agent*: …"), makes
  the submitter the thread initiator, and runs the question as the first turn. Slack hides
  developer slash commands in threads and in the agent pane, so the command only opens channel
  conversations; in a channel the bot is not a member of, the gateway joins public channels and
  asks for an invite to private ones. Failures (unknown agent, roster unavailable, channel not
  served) are reported privately to the invoking user.

A thread's agent binding is not re-derived after a restart — it does not need to be. It lives
in the thread's record in the routing store (see [Threads and sessions](#threads-and-sessions)),
so on a persistent store (`routing.store: valkey` or `bolt`) a restart changes nothing: same
agent, same initiator, same grants. The gateway never reads Slack history — no
`conversations.replies`, no re-parsing the opening message or the slash command's root — to
recover any of it; the routing-store record is the only carrier. On `routing.store: memory` a
restart loses this state exactly as it loses the instance bindings, and every thread starts
fresh from its next message.

The turn that opens a conversation — the first one, root or reply, that finds no agent
recorded — also posts the "🚀 Bringing in *Agent* to help…" launch announcement, once, in that
thread. A later turn never repeats it; neither does a DM, or the slash command's own branded
root, which already names the agent.

| Flag | Env var | Required |
|------|---------|---------|
| `--slack-default-agent` | `KLAUS_GATEWAY_SLACK_DEFAULT_AGENT` | Yes (when Slack is enabled) |

When `--driver=static`, the gateway validates at startup that the named agent exists in
the pre-configured instance set. With other drivers (klausctl, operator), the name is
used as the instance creation hint and instances may not exist yet at startup.

Example Helm values:

```yaml
slack:
  enabled: true
  defaultAgent: my-instance
```

## Running in Events API mode (production)

1. Create a Kubernetes Secret with the credentials:

    ```bash
    kubectl create secret generic slack-credentials \
      --namespace klaus-gateway \
      --from-literal=bot-token='<Bot User OAuth Token>' \
      --from-literal=signing-secret='<Signing Secret>'
    ```

2. Enable in Helm values:

    ```yaml
    slack:
      enabled: true
      mode: events
      secretName: slack-credentials
    ```

3. Set the **Request URL** in your Slack app (Settings → Event Subscriptions) to:

    ```
    https://<your-domain>/channels/slack/events
    ```

    Slack sends a URL verification challenge on save; the adapter handles it automatically.

## Running in Socket Mode (development)

Socket Mode uses a WebSocket connection; no public URL is required.

1. Enable Socket Mode in the Slack app (Settings → Socket Mode).
2. Create the App-Level Token (Settings → Basic Information → App-Level Tokens). Grant the
   `connections:write` scope.
3. Provide the credentials as environment variables or in a secrets file:

    ```bash
    export SLACK_BOT_TOKEN=<Bot User OAuth Token>
    export SLACK_SIGNING_SECRET=<Signing Secret>
    export SLACK_APP_TOKEN=<App-Level Token>
    ```

4. Start `klaus-gateway` with:

    ```bash
    ./bin/klaus-gateway \
      --slack-enabled \
      --slack-mode=socketmode
    ```

## Credential precedence

Credentials are resolved in this order (later sources win):

1. Secrets file (`--slack-secrets-file`, default `~/.config/klausctl/gateway/slack-secrets.yaml`)
2. Environment variables: `SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET`, `SLACK_APP_TOKEN`

The secrets YAML format:

```yaml
bot_token: <Bot User OAuth Token>
signing_secret: <Signing Secret>
app_token: <App-Level Token>   # socketmode only
```

`bot_token` and `signing_secret` are always required. `app_token` is only required in
Socket Mode.

**Note:** Do not commit Slack tokens to the repository. The CI gitleaks scan will fail on
any string that begins with `Slack bot`, `Slack app-level`, or `Slack user`.

## Message flow

1. Slack delivers an event to `/channels/slack/events` (Events API) or via WebSocket
   (Socket Mode).
2. The adapter ignores bot messages and messages with a subtype, with one exception:
   `thread_broadcast` (a thread reply the author also sent to the channel) is a normal human
   reply and is routed like one. It processes `app_mention` and `message.im` events only.
3. The `@mention` prefix is stripped from `app_mention` text before routing.
4. The routing key is `(channel="slack", channelID=<Slack channel ID>, userID=<Slack user ID>,
   threadID=<thread_ts or ts>)`.
5. A stable A2A contextID is derived from `(channel, channelID, "", threadID, agentRef)` with
   an empty user slot, so the same thread always maps to the same contextID for every
   participant, allowing Klaus to resume the conversation. See
   [Threads and sessions](#threads-and-sessions).
6. The gateway forwards the turn through the A2A executor to the instance named by
   `slack.defaultAgent`. The OpenAI `/v1` path is bypassed.
7. Progress is shown by adding a working reaction to the triggering message. On success the
   working reaction is removed with no residual emoji (default); set
   `SLACK_CLEAR_REACTION_ON_DONE=false` to swap in a done reaction instead. A failed turn always
   swaps in the failed reaction. With `SLACK_PROGRESS_MODE=text`, or in `auto` mode when
   `reactions:write` is unavailable, a `_thinking…_` placeholder message is posted instead.
8. Completion deltas are batched into a Block Kit `markdown` block and written back via
   `chat.update` (or an initial `chat.postMessage`) as the response accumulates. Replies over
   12,000 characters roll over into follow-up in-thread messages on code-fence boundaries; the
   message's notification fallback text is cut to Slack's 4,000-character limit for that field.
   Each streamed text run is rendered once — the A2A artifact update's append/replace semantics
   are honoured, so the Go ADK's re-send of a finished run does not duplicate it — and runs
   separated by tool calls are separated by a paragraph. A Slack refusal while rendering never
   fails the turn: flushes keep retrying until the agent finishes, a message refused as too long
   is re-split smaller, and only a final flush that still fails is reported in the thread (the
   reply is incomplete, with the failed reaction) while the turn still counts as completed.

Turns are serialized per thread: a message that arrives while the thread's previous turn is
still running gets a brief "still working" notice rather than starting an overlapping turn.
A signed-out sender's message is held for sign-in instead (no busy notice) and replays once
they link and the running turn finishes.

### Records

Three structured log records (`record=…`, JSON fields) tell a turn's story; join them on
`thread_id` (the Slack `thread_ts`) and `trace_id`:

- `turn_dispatch` -- the turn is admitted and about to be sent: `agent`, `agent_source`
  (`prefix`, `command`, `thread`, `default`, `task`), `slack_user`, `subject` (the resolved
  e-mail), `sub` (the linked muster identity), `channel_id`, `thread_id`, `message_id`,
  `task_id` (on a resume), `resume`, `trace_id`, `dispatch_ms` (since the event arrived).
- `turn_complete` -- one per turn, whatever its end: `outcome` (see
  [deployment.md](deployment.md#observability) for the values), `agent`, `slack_user`,
  `subject`, `channel_id`, `thread_id`, `message_id`, `task_id` (the A2A task the controller
  named), `tool_calls`, `streamed_chars`, `trace_id`, `error` on a failure, and the phases as
  milliseconds since the events POST (or the Socket Mode frame) arrived: `token_mint_ms`,
  `roster_ms`, `intro_post_ms`, `dispatch_ms`, `create_instance_ms`, `first_event_ms`,
  `first_text_ms`, `task_done_ms`, `stream_end_ms`, `final_flush_ms`, `total_ms`. A phase that
  did not happen (no instance created on a follow-up, no intro on a reply) is absent. A turn a
  previous process left running and this one delivered after a restart gets a record too, its
  timeline starting at the delivery.
- `token_refresh` -- the person's muster id_token was refreshed: `trigger` (`ahead` for the
  background refresher, `turn` for a refresh on the turn's path), `slackUser`, `duration_ms`,
  `expires_in_s`, or `error`. The refresher keeps the tokens of the people whose token a turn
  asked for in the last 48 hours fresh -- every minute it refreshes those within five minutes of
  expiry, under the same per-user lock and through the same store write as a turn's refresh, so
  the rotating refresh token is never raced -- and a turn's `token_mint_ms` stays at a cache hit.
  When the refresher could not reach muster in time the turn refreshes as before; a refresh
  muster refuses drops the link like a turn's would, and the person is asked to sign in on their
  next message.

The same phases feed the `klaus_gateway_turn_phase_seconds` histograms and the outcome the
`klaus_gateway_turn_total` counter; the trace the records name spans the gateway, the kagent
controller and the actor when `observability.otlpEndpoint` is set.

### Restarts and `/stop`

A turn ends early for one of two reasons, and the thread can tell them apart:

- **`/stop`** is the user's decision. The working reaction is cleared, the status ticker
  collapses into its receipt, nothing else is posted in reactions mode (`_(stopped)_` replaces
  the placeholder in text mode), and the task is cancelled at the controller so the agent
  stops working.
- **An error** before any answer text (an agent that did not start in time, a controller
  refusal) marks the triggering message with the failed reaction and posts
  `_(the turn failed; please try again)_` in the thread, in reactions mode too — the emoji
  alone does not say whether a retry helps, and for a conversation the gateway opened itself it
  sits on the bot's own root message. Once answer text has streamed, only the reaction marks
  the incomplete reply.
- **A gateway restart** (a pod restart, a node loss with a grace period) is nobody's decision.
  The thread gets a one-line notice — `⚠️ I was restarted while **<agent>** was working. It
  keeps going — the result is in the Dev Portal, and I post it here when it is done.` — the
  working reaction is cleared, the ticker collapses into its receipt, and the task is **left
  running** at the controller. The new gateway process resubscribes to it on start (A2A
  `SubscribeToTask` on the thread's AgentInstance, under the same user's freshly minted
  token) and streams what is left — or, when the task finished in between, posts the whole
  answer — into the thread, with the working reaction back on the original message while it
  does. A turn the start-up recovery cannot reach (its user signed out, the controller not up
  yet after three tries ten seconds apart) is delivered by the thread's next reply, ahead of
  that reply's own answer; a task the controller no longer has gets a short note instead.

The recovery rides on the thread's routing-store binding, which records the task in flight
while a turn runs. It therefore needs a routing store that outlives the process
(`routing.store: valkey` or `bolt`); with `memory` the record dies with the pod and
the notice says so ("I cannot bring it into this thread"). The pod's
`terminationGracePeriodSeconds` must leave room for the notice: the shutdown drains the HTTP
servers first (up to 15 s) and stops the Slack adapter after that (up to 15 s more), see
[deployment.md](deployment.md#shutdown-and-restarts).

### Progress configuration

| Flag | Env var | Default |
|------|---------|---------|
| `--slack-progress-mode` | `SLACK_PROGRESS_MODE` | `auto` (`reactions` with a text fallback), or `reactions` / `text` |
| `--slack-working-emoji` | `SLACK_WORKING_EMOJI` | `eyes` |
| `--slack-done-emoji` | `SLACK_DONE_EMOJI` | `white_check_mark` |
| `--slack-failed-emoji` | `SLACK_FAILED_EMOJI` | `x` |
| `--slack-clear-reaction-on-done` | `SLACK_CLEAR_REACTION_ON_DONE` | `true` (remove the working reaction without adding a done reaction) |

### Identity, HITL, and channel behavior

- **Per-message branding.** Agent replies, the agent's own confirmation prompts, and the launch
  announcement are posted under the agent's display name, so they read as the agent speaking
  rather than the app. The name is the `Agent` CR's `ui.giantswarm.io/display-name` annotation
  (as reported by the roster), falling back to the resource's own name — `sre-agent`, not the
  underscored `sre_agent` the AgentCard publishes. The AgentCard supplies only the icon, which
  therefore stays keyed to the technical name: renaming an agent relabels it without changing
  how it looks. When no icon is available the app's own icon is kept. Swarmgeist's other
  messages (sign-in, errors, the DM redirect, the channel intro) keep the app's default
  identity. Requires `chat:write.customize`.
- **Inspect agent steps.** By default a turn shows only a compact status ticker and a
  one-line tool receipt. To see the actual tool calls after the fact, invoke the
  **Inspect agent steps** message shortcut (⋯ menu → Apps) on any message in the thread:
  the gateway replies with an ephemeral, invoker-only rendering of the retained tool-call
  log — per call, the tool name with its arguments and a result preview, grouped per turn
  (the same content `/details full` streams live). The log is in-memory and bounded: the
  last 100 calls per thread, kept for up to 24 hours and not surviving a gateway restart;
  nothing is recorded while the thread is set to `/details off`. When nothing is retained
  the reply says so and points at `/details full` for live debugging. The shortcut is
  registered in `deploy/slack/manifest.yaml`; changing the manifest requires re-syncing
  the app config at api.slack.com/apps.
- **HITL "Chat".** A tool-approval prompt shows Approve / Deny / **Chat**. Chat holds the
  pending tool call and invites a follow-up question in the thread; the reply is routed to the
  paused task. A question resolves it as a reject carrying the question (the agent answers and
  asks to confirm again); a plain "approve"/"deny" reply still decides.
- **Channel intro.** When the bot is added to a channel it posts a one-time introduction
  (requires the `member_joined_channel` bot event).
- **Launch announcement.** A new channel thread opens with a short Swarmgeist hand-off notice
  before the agent takes over.
- **Sign-in prompt.** An unlinked user's first message is answered with a "Sign in to Giant
  Swarm" prompt. In a channel the prompt is ephemeral, so only that user sees the link; a
  short notice in the thread says the agent is waiting for a sign-in, names nobody and
  carries no link, and gives the ephemeral something to render against. The notice is
  posted once per thread and serves every unlinked user in it. In a DM the prompt is a
  real threaded message and lands in the Slack
  Assistant pane. Once the link completes, a DM prompt is rewritten in place to the
  signed-in confirmation, with the agent hand-off folded in when a held message is about to
  replay; a channel prompt cannot be rewritten (an ephemeral has no message id), so the
  confirmation is a fresh ephemeral to the same user. The sign-in link is per-user and the
  callback also verifies the OAuth identity's email against the Slack profile email. The
  link in the button expires after 15 minutes; a message sent after that gets a fresh
  prompt. A DM prompt is rewritten to say its link expired; a channel prompt cannot be
  rewritten, so the fresh ephemeral says the earlier link expired instead. Messages sent
  before
  signing in are held and replayed after the link completes; only the last 5 per thread are
  kept, and the user is told when earlier ones are dropped.
- **Transient sign-in failures.** When a linked person's token cannot be minted right now —
  muster's token endpoint or the gateway's link store not answering — they get an ephemeral
  "I couldn't refresh your Giant Swarm sign-in just now" notice and their message is not held;
  the sign-in prompt is reserved for people with no link. `/login` answers the same way, and
  `/logout` reports a sign-out the link store refused instead of confirming it. The gateway
  keeps a process-local copy of every link it has served, so a store outage does not reach the
  people it already knows and a refresh token the store failed to take is written later rather
  than lost (see `deployment.md`, "OBO link store").
- **One shared session per thread.** A thread maps to a single agent session. On the
  current kagent (v0.9.9) that session acts under the thread initiator's identity even after
  others are allowed in; a granted collaborator instructs the agent on the initiator's
  behalf. Per-user identity within one shared session is a kagent gap (kagent-dev/kagent#1933
  and #2181); until it lands, actions are attributed to the initiator.
- **Surfaces.** DMs and channels are controlled independently. `SLACK_DM_MODE` selects the DM
  behaviour: `serve` (answer DMs, the default), `redirect` (a polite pointer to channels), or
  `ignore` (drop silently). `SLACK_CHANNEL_MODE` selects the channels served: `all` (every
  channel the bot is invited to, the default), `allowlist` (only the channel IDs in
  `SLACK_CHANNEL_ALLOWLIST`, comma-separated), or `none` (DM-only deployments). A mention in a
  channel outside the allowlist gets a one-time ephemeral notice; the channel intro and all
  other activity are suppressed there.

## Required bot OAuth scopes

| Scope            | Purpose                                               |
|------------------|-------------------------------------------------------|
| `chat:write`     | Post messages and update existing messages            |
| `chat:write.customize` | Post agent replies under the agent's own name/icon |
| `reactions:write` | Add/remove progress reactions on the triggering message |
| `im:history`     | Read DMs sent to the bot                              |
| `channels:history` | Required for Slack to deliver `message.channels` events (channel messages) to the bot; the gateway does not read channel history |
| `channels:join`  | Join public channels on invite                        |
| `files:read`     | Download message attachments (`url_private`) to forward to the agent |

The `member_joined_channel` bot event must also be subscribed for the channel intro.

## Endpoints

The adapter mounts three routes in events mode (none in socketmode, where the same payloads
arrive as Socket Mode envelopes):

```
POST /channels/slack/events        Events API webhook
POST /channels/slack/interactions  Block Kit clicks, the message shortcut, the agent picker's view_submission
POST /channels/slack/commands      the slash command
```

Every endpoint verifies the `x-slack-signature` HMAC header using the signing secret and acks
within Slack's 3-second window before doing any work. The events endpoint also answers
`url_verification` challenges with the `challenge` value.
