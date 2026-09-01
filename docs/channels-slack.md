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

In pane threads the live tool ticker (`⏳ tool… · step N`) renders as Slack's
**native status indicator** under the composer (`assistant.threads.setStatus`,
requires the `assistant:write` scope from the manifest) instead of a message,
so while the agent works the user sees the native "working…" line and the only
ticker artifact left in the thread is the collapsed receipt (`🛠️ N steps · …`)
posted when a segment closes or the turn ends. The status is cleared explicitly
on turn end, error, and prompt pause (Slack also auto-clears it on the app's
next in-thread post and after two idle minutes). Installs where `setStatus` is
unavailable — the scope missing from the bot token, or a non-Agent app whose
DMs are plain conversations — downgrade automatically to the message ticker for
the rest of the process lifetime. Channels always use the message ticker:
`setStatus` does not exist there.

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

## Agent routing

Every Slack thread is routed to a single Klaus instance via the A2A executor. The target
agent is fixed at startup using `slack.defaultAgent`.

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
   12,000 characters roll over into follow-up in-thread messages on code-fence boundaries.

Turns are serialized per thread: a message that arrives while the thread's previous turn is
still running gets a brief "still working" notice rather than starting an overlapping turn.
A signed-out sender's message is held for sign-in instead (no busy notice) and replays once
they link and the running turn finishes.

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
- **HITL "Chat".** A tool-approval prompt shows Approve / Deny / **Chat**. Chat holds the
  pending tool call and invites a follow-up question in the thread; the reply is routed to the
  paused task. A question resolves it as a reject carrying the question (the agent answers and
  asks to confirm again); a plain "approve"/"deny" reply still decides.
- **Channel intro.** When the bot is added to a channel it posts a one-time introduction
  (requires the `member_joined_channel` bot event).
- **Launch announcement.** A new channel thread opens with a short Swarmgeist hand-off notice
  before the agent takes over.
- **Sign-in prompt.** An unlinked user's first message is answered with a "Sign in to Giant
  Swarm" message posted as a threaded reply, addressed to that user (in channels it anchors
  the conversation thread; in a DM it lands in the Slack Assistant pane). Once the link completes the same message is
  updated in place to the signed-in confirmation, with the agent hand-off folded in when a
  held message is about to replay. The sign-in link is per-user and the callback verifies the
  OAuth identity's email against the Slack profile email, so a prompt visible to the whole
  thread cannot be completed by someone else. The link in the button expires after 15
  minutes; a message sent after that gets a fresh prompt, and the old one is rewritten to
  say its link expired. Messages sent before signing in are held and replayed after the
  link completes; only the last 5 per thread are kept, and the user is told when earlier
  ones are dropped.
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
| `channels:history` | Read messages in channels the bot is a member of   |
| `channels:join`  | Join public channels on invite                        |
| `files:read`     | Download message attachments (`url_private`) to forward to the agent |

The `member_joined_channel` bot event must also be subscribed for the channel intro.

## Endpoint

The adapter mounts a single route:

```
POST /channels/slack/events    Events API webhook (events mode only; no-op in socketmode)
```

The endpoint:
- Verifies the `x-slack-signature` HMAC header using the signing secret.
- Responds to `url_verification` challenges with the `challenge` value.
- Dispatches `event_callback` payloads to the Klaus instance asynchronously.
