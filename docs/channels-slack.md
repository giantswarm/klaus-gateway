# Slack channel adapter

The Slack adapter lets workspace members talk to Klaus by mentioning the bot or sending it
a direct message. It is disabled by default and must be enabled explicitly.

Two connection modes are supported:

| Mode         | Value        | When to use                                           |
|--------------|-------------|-------------------------------------------------------|
| Events API   | `events`    | Production. Requires a public HTTPS webhook URL.      |
| Socket Mode  | `socketmode`| Development. No public URL required.                  |

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
2. The adapter ignores bot messages and messages with a subtype; it processes `app_mention`
   and `message.im` events only.
3. The `@mention` prefix is stripped from `app_mention` text before routing.
4. The routing key is `(channel="slack", channelID=<Slack channel ID>, userID=<Slack user ID>,
   threadID=<thread_ts or ts>)`.
5. The gateway resolves the routing key to a Klaus instance (creating one if auto-create is
   enabled).
6. A placeholder `_thinking…_` message is posted to the Slack thread immediately.
7. Completion deltas are batched and written back via `chat.update` calls as the response
   accumulates.

## Required bot OAuth scopes

| Scope            | Purpose                                               |
|------------------|-------------------------------------------------------|
| `chat:write`     | Post messages and update existing messages            |
| `im:history`     | Read DMs sent to the bot                              |
| `channels:history` | Read messages in channels the bot is a member of   |
| `channels:join`  | Join public channels on invite                        |

## Endpoint

The adapter mounts a single route:

```
POST /channels/slack/events    Events API webhook (events mode only; no-op in socketmode)
```

The endpoint:
- Verifies the `x-slack-signature` HMAC header using the signing secret.
- Responds to `url_verification` challenges with the `challenge` value.
- Dispatches `event_callback` payloads to the Klaus instance asynchronously.
