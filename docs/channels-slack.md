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
normal end, stream error, `/stop`. Every turn makes that exit call, right after
the `chat.stopStream` that closes its answer. The stop names the same status in
its own `session_status` field, but Slack was observed (graveler, 2026-09-21) to
accept that field without clearing the indicator, so the status call is what
ends the session. A turn that pauses on a HITL prompt — an
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

The indicator carries no text of its own — it says only *that* the agent is
working. What it is working on is said by the reply's own **step list** (see
[Message flow](#message-flow)), so the two are independent: installs
where the method is unavailable — the scope missing from the bot token, the
wrong token type, or agent messaging disabled for the workspace — drop the
native indicator for the rest of the process lifetime and keep the steps. A
`not_authorized` rejection (the bot is not a member of that one channel) only
costs that call.

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
session. Should Slack refuse the decorated call, the status is sent again bare,
so a refused title never costs the turn its indicator.

The creating call also **names the session's starter** (`initiator_user_id`):
the thread's initiator, the owner the access rule already keeps in the thread's
routing-store row, falling back to the turn's sender when the turn carries no
owner. Slack applies it on creation only, like the title, so it rides on the
same calls — a turn's `processing` call, or a status set from outside a turn,
which can create a session too — and a thread whose owner is unknown sends
none. It needs no scope beyond the `chat:write` the call already uses.

The same line names the thread's kagent conversation, so both surfaces list the
thread under what was asked in it (see `docs/kagent-a2a.md`). That title travels
on every turn, not just the opener's: the gateway names a conversation when it
creates one, which a turn that switches agents does mid-thread.

### Threads and conversations

- `threadID` is `thread_ts` if set, otherwise the message `ts`.
- A thread is bound to exactly one agent and exactly one **AgentInstance** — the
  running conversation the kagent controller creates and owns, which holds the
  agent's memory of that thread (see [kagent A2A](kagent-a2a.md)). The thread's
  first turn creates the instance; the create is idempotent per person and
  thread, so a retried first turn — the binding was not written, the process died
  in between — gets the same instance back instead of a second one. The instance
  id and the agent ref are written into the thread's row, every later turn of the
  thread is addressed to that instance, and a gateway restart keeps the mapping on a
  persistent store (`valkey` or `bolt`), because the row carries it, not the process.
- In a DM every top-level message opens its own thread and therefore its own
  instance; a "New chat" in the assistant pane likewise.
- A turn runs under the thread **initiator's** identity. For a granted
  collaborator's turn the gateway forwards the initiator's token and attaches the
  collaborator as attribution, so the instance is created and addressed by one
  principal and the agent still sees who spoke while acting with the initiator's
  rights. That is why a newcomer needs the initiator's consent — the access
  prompt — before their message runs. The initiator's own turns, and turns where
  the initiator's token cannot be minted, use the sender's own token rather than
  the gateway service account (`applyInitiatorIdentity` in
  `pkg/channels/slack/slack.go`).
- **When the instance is gone.** Two cases clear only the binding fields of the row;
  the thread keeps its agent, its initiator and its grants, and the next turn creates
  a fresh instance. An instance the controller no longer has is found by the resume
  check that runs on the first reply this process sees in a thread: that reply gets
  the starting-fresh notice ("I couldn't find our earlier conversation in this thread,
  so I'm starting fresh."), and a loss found in the middle of a conversation posts no
  notice. A `ResetSession` after a corrupt history posts the corrupt-session notice
  instead — "An earlier interrupted turn corrupted this conversation's history … I've
  reset the session: please resend your message …" — so the person knows to resend.
- **The thread's earlier messages go to the agent.** When a conversation opens inside a thread that
  already has messages — the **Ask an agent here** shortcut, an `/agent "<name>" <question>` reply,
  a bare mention under an alert — the adapter reads that thread once, on the opening turn, and hands
  the messages written before the opener to the agent as a labelled part of its own (`[thread
  context shared by <name>: N earlier messages in this thread, oldest first]`, one line per message
  with a UTC time and the author's display name). A bot's alert is flattened out of its attachments
  and blocks, which is where PagerDuty and friends put the text; `<@U…>` mentions become names; the
  gateway's own posts and content-less events are left out; files are named, never downloaded. At
  most 12,000 characters, oldest dropped first, root always kept — a constant, not configuration;
  there is no message limit, so a thread of many short messages is handed over whole. The read
  itself is bounded to ten pages of the Slack API (about 500 messages); a thread longer than that is
  labelled as a partial read rather than presented as its newest messages. The picker offers a
  checkbox ("Include the earlier messages in this thread", ticked) so the person starting the
  session can leave a noisy thread out; it carries no count, because counting would mean reading the
  thread before the modal opens and Slack kills the trigger after three seconds. The reply entry
  points have no modal and no checkbox. A conversation that starts its own thread (a mention on a
  root, the slash command) has nothing earlier to read, and a DM is never read at all, whichever
  entry point opened the conversation — the shortcut included, which Slack also offers in a DM: the
  assistant pane roots every chat at an anchor of Slack's own, so there is no thread there that
  predates the conversation, and the only earlier messages are the person's own and the bot's.
  Nothing is posted in the thread for this, and later turns read nothing: they are turns of the
  conversation already. One 5-second budget covers every call the transcript costs — the paged
  thread read and the display-name lookups behind it — so a rate-limited Slack cannot hold the first
  reply: past it an author is named by their Slack ID, and a read that failed outright leaves the
  turn running without a transcript and the person who opened the conversation with one ephemeral
  naming the reason.
- Each thread's durable state — its agent, its initiator, the collaborators the initiator
  allowed, and its AgentInstance binding — lives in one row in the routing store, at the
  thread's plain key (`slack|<channelID>||<threadID>`, the user slot empty). It is the only
  carrier: the gateway never reads Slack history to recover any of it — the context read above is a
  different read, for the agent's benefit, and nothing it returns is ever written back. The row has one sliding
  lifetime — `routing.threadTTL` (`--thread-ttl`), 90 days by default, `0` never expires —
  refreshed by every turn. While the thread lives, the initiator and the collaborators they
  allowed instruct the agent without mentioning the bot again and their grants hold. After that
  long without a message the **conversation has ended**: the agent, the initiator, the grants and
  the binding all read as absent, an un-mentioned reply is not answered, and the next mention
  starts the thread over — its author becomes the initiator and no grant carries over. Whether
  the agent remembers is the controller's call: its create is idempotent per person and thread,
  so the same person gets the earlier instance back while the controller still holds it, and
  another person gets a new one.
- **A reply in a conversation that ended is told so.** The row itself stays in the store for
  twice the lifetime (180 days by default; `0` still never expires), and while it is there the
  gateway knows the difference between a thread whose conversation ended and a thread it was
  never in. So an un-mentioned reply in one of the former gets one private line — _"This
  conversation ended after 90 days without messages. Mention me to start a new one."_ — instead
  of silence. The sentence names the configured lifetime exactly, as the count of the largest
  unit it is a whole multiple of: `--thread-ttl=36h` reads "36 hours", not "1 day", and `90m`
  reads "90 minutes". It is ephemeral, so only its author sees it, every reply gets it, and
  nothing is written to the store for it. A reply that **does** mention the bot gets no notice —
  it starts the conversation over, and Slack delivers it twice (as a mention and as a plain
  message), so the notice would answer the mention with a request to mention. Past twice the
  lifetime the row is gone and the thread is a stranger again: replies are ignored without a
  word, as for any thread the bot was never in. There is no warning before the end, and no
  sweep: the notice is posted when somebody writes, which is the moment it is useful. A
  thread's row adopts the configured lifetime on its next message.

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
gateway does not depend on it), two message shortcuts (**Inspect agent steps** and **Ask an agent
here**; their display names are per app too, the gateway routes on the callback id) and the
`commands` scope they need. Manifest changes are applied by
hand at api.slack.com/apps; adding the `commands` scope to an install that lacks it requires a
reinstall.

## Agent routing

Every Slack thread is routed to a single agent via the A2A executor. A conversation picks its
agent when it opens, through one of three entry points, and keeps it for life:

- **Mention with a prefix**: `@bot /agent "<display name>" <question>` or
  `@bot /agent <technical-name> <question>` starts a conversation in any thread with no agent
  recorded yet — a root `@`-mention, or a reply inside an existing thread that has none of its
  own (an alert another app posted, say). The technical name may carry the served
  namespace (`kagent/sre-agent`); it names the same agent as the bare name. Without a prefix the conversation goes to the default
  agent (`slack.defaultAgent`). Inside a thread that already has a conversation, naming its own
  agent again is a no-op — the turn dispatches as a normal reply — and naming a different agent
  is refused: the thread's row binds one agent and one AgentInstance, and a different agent
  would need a different instance, so the switch is refused rather than forking the
  conversation.
- **Slash command**: `/swarmgeist [question]` in a channel opens a modal with an agent select over
  the live roster (the default agent preselected) and a question box. On submit the gateway posts
  the conversation root itself, under the agent's identity ("💬 @user asked *Agent*: …"), makes
  the submitter the thread initiator, and runs the question as the first turn. Slack hides
  developer slash commands in threads and in the agent pane, so the command only opens channel
  conversations; in a channel the bot is not a member of, the gateway joins public channels and
  asks for an invite to private ones. Failures (unknown agent, roster unavailable, channel not
  served) are reported privately to the invoking user.
- **"Ask an agent here" message shortcut** (⋯ menu → Apps on any message): opens the same picker
  where the command cannot reach — inside an existing thread. The conversation starts in the
  thread of the message the shortcut was invoked on (or in the thread that message roots, when it
  is a top-level one), so an alert another app posted or a running discussion is handed to a
  chosen agent without leaving it. On submit the gateway posts the same "💬 @user asked *Agent*:
  …" echo as a **reply** in that thread, makes the submitter the thread initiator, and runs the
  question as the first turn. Two kinds of thread are refused, with nothing posted: one that
  already talks to an agent — reply in it to ask that agent, a second conversation would fork the
  one it has — and one that already belongs to someone else (a `/usage` or `/stop` typed there
  made them its initiator) — reply in it, so the owner is asked to allow you, since a conversation
  opened by the picker would run under the owner's delegated identity. Refusals and failures
  are private to the invoker, like the command's. The shortcut works in DMs too when DMs are
  served.

An agent that is installed but that no Harness admits, or whose compiled revision is not ready,
is refused with that reason instead of as an unknown name, on the technical-name form and on
the pickers. The quoted display-name form still reports an unknown name in that case, because
it resolves the name against the roster, which lists selectable agents only.

A thread's agent binding is not re-derived after a restart — it does not need to be. It lives
in the thread's row in the routing store (see [Threads and conversations](#threads-and-conversations)),
so on a persistent store (`routing.store: valkey` or `bolt`) a restart changes nothing: same
agent, same initiator, same grants. The gateway never reads Slack history — no
`conversations.replies`, no re-parsing the opening message or the slash command's root — to
recover any of it; the routing-store row is the only carrier. On `routing.store: memory` a
restart loses this state, and every thread starts fresh from its next message. On any store a
thread nobody has written in for `routing.threadTTL` has ended, agent and all, and its next
mention starts it over (see [Threads and conversations](#threads-and-conversations) for what the agent may
still remember and for the notice an un-mentioned reply gets meanwhile).

The turn that opens a conversation posts no notice of its own: the agent's first reply, under
the agent's name, is the first sign of which agent joined the thread. The picker's branded echo
(the slash command's root, the shortcut's reply) names the agent up front, because there the
agent was chosen before any message existed.

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
5. The thread's row in the routing store names its agent and its AgentInstance. The thread's
   first turn creates that instance and records it; every later turn is addressed to it, so
   every participant in the thread talks to the one conversation. See
   [Threads and conversations](#threads-and-conversations).
6. The gateway forwards the turn through the A2A executor to the thread's AgentInstance — the
   agent the row names, which is the default agent when nothing selected another one. The
   OpenAI `/v1` path is bypassed.
7. Progress is shown by adding a working reaction to the triggering message. On success the
   working reaction is removed with no residual emoji (default); set
   `SLACK_CLEAR_REACTION_ON_DONE=false` to swap in a done reaction instead. A failed turn always
   swaps in the failed reaction. With `SLACK_PROGRESS_MODE=text`, or in `auto` mode when
   `reactions:write` is unavailable, a `_thinking…_` placeholder message is posted instead.
8. The whole turn is streamed into **one** Slack message with the streaming API:
   `chat.startStream` opens it, `chat.appendStream` adds what has accumulated since the last
   tick (one second), and `chat.stopStream` closes it with the answer's last words, naming
   the session's exit status. Slack animates the message while the stream is open.

   The stream carries a list of typed **chunks**, not a plain text field — a message uses one
   of the two from its first call to its last, and Slack refuses a mode change mid-message —
   so everything the turn produces queues up in the order the agent produced it and goes out
   together:

   - the answer, and the agent's interim narration (the prose it writes before firing a tool
     call), as `markdown_text` chunks;
   - each tool call as a `task_update` chunk, which Slack renders as a **step** of a task list
     attached to the reply: `in_progress` when the call starts, `complete` — or `error` when
     the tool reported one — when its result arrives. The two updates share an id, so Slack
     replaces the step rather than listing the call twice. Slack collapses the list once the
     answer is done.

   The stream therefore opens at the **first** thing the turn produces — a tool call, a
   narration passage or the first answer text, whichever comes first — because tools usually
   run before any answer text and the steps have to live in the reply.

   Step titles are plain language, as Slack's agent design guide asks: muster's meta-tools get
   phrases of their own (`filter_tools` → "Finding the right tool"), a `call_tool` wrapper is
   unwrapped to the tool it really runs, and every other name is humanised by dropping the
   `x_`/`workflow_` namespace and capitalising the rest (`x_kubernetes_list` → "Kubernetes
   list"). `/details` decides how much a step carries:

   | `/details` | What a step shows |
   |------------|-------------------|
   | `off`      | nothing — no step is rendered and nothing is recorded (the private mode) |
   | `on`       | the title alone (the default) |
   | `full`     | the title, plus the raw tool name and its arguments as the step's details and the result preview as its output, each cut to Slack's 256-character chunk limit |

   The **Inspect agent steps** shortcut is the audit view at `on` and `full` alike, with the
   fuller payloads; `/details off` still records nothing for it.

   Each append carries only what is new, and answer text is sent up to the last whitespace
   boundary — an unfinished word waits for the next append, so nothing is ever half-written.
   Replies over 12,000 characters roll over into a further streamed message; the intermediate
   close carries `processing`, so the working indicator stays on mid-answer. Narration counts
   toward that per-message limit like any other prose, but never toward the answer length the
   delivery record carries — a process continuing the turn after a restart replays the answer,
   not the narration, so the two are counted separately. In a channel the stream names the
   person it answers (`recipient_user_id` + `recipient_team_id`); in a DM it names nobody,
   which is what Slack requires there. Each streamed text run is rendered once — the A2A
   artifact update's append/replace semantics are honoured, so the Go ADK's re-send of a
   finished run does not duplicate it — and runs separated by tool calls are separated by a
   paragraph. In text-progress mode the `_thinking…_` placeholder is removed once the streamed
   message exists (a stream cannot take over an existing message). A Slack refusal while
   rendering never fails the turn: flushes keep retrying until the agent finishes, a message
   Slack closed under the app gets one replacement stream, and only a final flush that still
   fails is reported in the thread (the reply is incomplete, with the failed reaction) while
   the turn still counts as completed. Pressing Slack's stop button ends the stream on Slack's
   side: the adapter learns it from the `stopped_by_user` its next call is answered with and
   stops writing — quietly, since the button's own "Stopped by @user" notice already tells
   the thread. `/metrics` counts the lifecycle as
   `klaus_gateway_slack_stream_total{event}` (`started`, `stopped`, `stopped_by_user`,
   `recovered`); `chat.startStream` and `chat.stopStream` are tier-2 methods, about 20 calls
   a minute for the whole app, so those rates are what says how close a workspace is to the
   ceiling.

   Throttling itself is counted as `klaus_gateway_slack_rate_limited_total{method,outcome}`:
   one per call made through the Web API client that Slack answers with a 429, under the
   method it answered (`method`) and what the client did about it (`outcome`) — `retried`, it
   waited the `Retry-After` and called again, so nothing failed; or `exhausted`, it gave up,
   because the four attempts ran out or the requested wait was over the 30-second cap, and the
   call failed with `rate limited`. Three Slack HTTP paths do not go through that client and
   are therefore not counted: the Socket Mode `apps.connections.open` handshake, the ephemeral
   posts to a slash command's `response_url`, and the `url_private` file downloads. A retried
   429 leaves no other trace, so any increase here is the early
   warning that a workspace is being paced: alert on
   `sum(rate(klaus_gateway_slack_rate_limited_total[5m])) > 0` for five minutes, then read the
   `method` label to see which call is being held back. Each 429 also writes a debug log line
   with the method, the wait and the attempt.

Turns are serialized per thread: a message that arrives while the thread's previous turn is
still running gets a brief "still working" notice rather than starting an overlapping turn; the
notice names `/stop`, and a reply that is just `stop` there interrupts the running turn like `/stop`.
A signed-out sender's message is held for sign-in instead (no busy notice) and replays once
they link and the running turn finishes.

### Records

Three structured log records (`record=…`, JSON fields) tell a turn's story; join them on
`thread_id` (the Slack `thread_ts`) and `trace_id`:

- `turn_dispatch` -- the turn is admitted and about to be sent: `agent`, `agent_source`
  (`prefix`, `command`, `shortcut`, `thread`, `default`, `task`), `slack_user`, `subject` (the resolved
  e-mail), `sub` (the linked muster identity), `channel_id`, `thread_id`, `message_id`,
  `task_id` (on a resume), `resume`, `trace_id`, `dispatch_ms` (since the event arrived).
- `turn_complete` -- one per turn, whatever its end: `outcome` (see
  [deployment.md](deployment.md#observability) for the values), `agent`, `slack_user`,
  `subject`, `channel_id`, `thread_id`, `message_id`, `task_id` (the A2A task the controller
  named), `tool_calls`, `streamed_chars`, `trace_id`, `error` on a failure, and the phases as
  milliseconds since the events POST (or the Socket Mode frame) arrived: `token_mint_ms`,
  `roster_ms`, `dispatch_ms`, `create_instance_ms`, `first_event_ms`,
  `first_text_ms`, `task_done_ms`, `stream_end_ms`, `final_flush_ms`, `total_ms`. A phase that
  did not happen (no instance created on a follow-up) is absent. A turn a
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

- **`/stop`** is the user's decision. The working reaction is cleared, the reply's stream is
  closed where it stands (its steps stay as they were), nothing else is posted in reactions
  mode (`_(stopped)_` replaces the placeholder in text mode), and the task is cancelled at the
  controller so the agent stops working.
- **An error** before any answer text (an agent that did not start in time, a controller
  refusal) marks the triggering message with the failed reaction and posts
  `_(the turn failed; please try again)_` in the thread, in reactions mode too — the emoji
  alone does not say whether a retry helps, and for a conversation the gateway opened itself it
  sits on the bot's own root message. Once answer text has streamed, only the reaction marks
  the incomplete reply.
- **A gateway restart** (a pod restart, a node loss with a grace period) is nobody's decision.
  The thread gets a one-line notice — `⚠️ I was restarted while **<agent>** was working. It
  keeps going — the result is in the Dev Portal, and I post it here when it is done.` — the
  working reaction is cleared, the reply's stream is closed where it stands, and the task is
  **left running** at the controller. The new gateway process resubscribes to it on start (A2A
  `SubscribeToTask` on the thread's AgentInstance, under the same user's freshly minted
  token) and streams what is left into the thread, with the working reaction back on the
  original message while it does. The answer text arrives whole when the task completes (the
  resubscription does not replay what streamed before it), so the process continues the
  reply where its predecessor left it rather than repeating it: the thread's row records, as
  a turn streams, how much answer text has landed, which streamed message it is landing in,
  and how many step ids have been handed out; the continuing process posts only the text after
  that mark — without the paragraph break the cut leaves in front — and numbers its own steps
  on from the recorded count, so no id already on the reply is reused. The step the previous
  process was running when it died would otherwise spin forever, so it is closed first; the
  record does not carry its title, so it closes as a plain "Step N". A message the previous
  process left open is adopted, so the reply goes on in the same bubble; if Slack closed it in
  the meantime the
  rest opens a message of its own. An adopted message is always closed, even when nothing is
  left to add, so it stops animating. A graceful restart closes the streamed message on its
  way out, so a continuation after one always opens a new message. A turn whose whole answer
  had landed before the restart closes with `_(done — the reply above is complete)_`. A turn the start-up recovery cannot reach (its user signed out,
  the controller not up yet after three tries ten seconds apart) is delivered by the thread's
  next reply, ahead of that reply's own answer; a task the controller no longer has gets a
  short note instead.

The recovery rides on the thread's routing-store binding, which records the task in flight
while a turn runs, and with it what of the reply has landed (`delivered`: the answer text's
length in bytes, the streamed message and its length, and the count of step ids handed out,
written after every flush and every step). It therefore needs a routing store that outlives the process
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

- **Per-message branding.** Agent replies and the agent's own confirmation prompts are posted
  under the agent's display name, so they read as the agent speaking
  rather than the app. The name is the `Agent` CR's `ui.giantswarm.io/display-name` annotation
  (as reported by the roster), falling back to the resource's own name — `sre-agent`, not the
  underscored `sre_agent` the AgentCard publishes. The AgentCard supplies only the icon, which
  therefore stays keyed to the technical name: renaming an agent relabels it without changing
  how it looks. When no icon is available the app's own icon is kept. An agent whose display
  name is the app's own name (the bot user's profile name, or its handle when `users:read` is
  not granted; compared case-insensitively) is not branded at all: it posts under the app
  identity, so the app and its namesake agent — typically the default agent — never appear as
  two faces with one name in a thread. Swarmgeist's other messages (sign-in, errors, the DM
  redirect, the channel intro) keep the app's default identity. Requires `chat:write.customize`.
- **Inspect agent steps.** By default a turn's step list names what the agent did, not what
  it sent or got back. To see the actual tool calls after the fact, invoke the
  **Inspect agent steps** message shortcut (⋯ menu → Apps) on any message in the thread:
  the gateway replies with an ephemeral, invoker-only rendering of the retained tool-call
  log — per call, the tool name with its arguments and a result preview, grouped per turn,
  fuller than what a step at `/details full` has room for. The log is in-memory and bounded:
  the last 100 calls per thread, kept for up to 24 hours and not surviving a gateway restart;
  nothing is recorded while the thread is set to `/details off`. When nothing is retained
  the reply says so and points at `/details full` for live debugging. The shortcut is
  registered in `deploy/slack/manifest.yaml`, next to **Ask an agent here** (which starts a
  conversation in the message's thread, see [Agent routing](#agent-routing)); changing the
  manifest requires re-syncing the app config at api.slack.com/apps. Slack lists a shortcut
  under "Connect to apps" in the ⋯ menu only once a person has used it; the first time it is
  behind "More message shortcuts…".
- **HITL "Chat".** A tool-approval prompt shows Approve / Deny / **Chat**. Chat holds the
  pending tool call and invites a follow-up question in the thread; the reply is routed to the
  paused task. A question resolves it as a reject carrying the question (the agent answers and
  asks to confirm again); a plain "approve"/"deny" reply still decides.
- **Channel intro.** When the bot is added to a channel it posts a one-time introduction
  (requires the `member_joined_channel` bot event).
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
- **One conversation per thread.** A thread is bound to one agent and one AgentInstance, and
  every turn in it runs under the thread initiator's identity even after others are allowed
  in; a granted collaborator instructs the agent on the initiator's behalf, with their own
  identity attached as attribution. Actions are therefore attributed to the initiator (see
  [Threads and conversations](#threads-and-conversations)).
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
| `chat:write`     | Post messages, update them, and stream agent replies  |
| `chat:write.customize` | Post agent replies under the agent's own name/icon |
| `reactions:write` | Add/remove progress reactions on the triggering message |
| `im:history`     | Read DMs sent to the bot                              |
| `channels:history` | Required for Slack to deliver `message.channels` events (channel messages) to the bot, and to read a public-channel thread when a conversation opens inside it |
| `groups:history` | The same read in a private channel                     |
| `mpim:history`   | The same read in a group DM                            |
| `channels:join`  | Join public channels on invite                        |
| `files:read`     | Download message attachments (`url_private`) to forward to the agent |

The `member_joined_channel` bot event must also be subscribed for the channel intro.
`groups:history` and `mpim:history` are new: Slack grants a scope only on re-install, so an app
installed before them keeps working and a thread in a private channel or a group DM answers
`missing_scope` — the conversation opens and runs, without the thread's earlier messages.

## Endpoints

The adapter mounts three routes in events mode (none in socketmode, where the same payloads
arrive as Socket Mode envelopes):

```
POST /channels/slack/events        Events API webhook
POST /channels/slack/interactions  Block Kit clicks, the message shortcuts, the agent picker's view_submission
POST /channels/slack/commands      the slash command
```

Every endpoint verifies the `x-slack-signature` HMAC header using the signing secret and acks
within Slack's 3-second window before doing any work. The events endpoint also answers
`url_verification` challenges with the `challenge` value.
