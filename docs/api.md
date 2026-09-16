# klaus-gateway API

klaus-gateway exposes two HTTP surfaces on its public mux. Neither requires a Klaus-specific SDK: the OpenAI-compat path works with any OpenAI client, and the web adapter is a plain bytes-in / SSE-out HTTP surface.

## OpenAI-compatible front door

### `POST /v1/{instance}/chat/completions`

Accepts a standard OpenAI `chat/completions` request body. When the body sets `"stream": true`, the response is forwarded as `text/event-stream` **byte-identical** to what the upstream klaus instance produced. The gateway does not re-encode deltas.

Request:

```http
POST /v1/test-instance/chat/completions HTTP/1.1
Content-Type: application/json

{
  "stream": true,
  "messages": [{"role": "user", "content": "ping"}]
}
```

Response:

```
HTTP/1.1 200 OK
Content-Type: text/event-stream

data: {"choices":[{"delta":{"content":"pon"}}]}

data: {"choices":[{"delta":{"content":"g"}}]}

data: [DONE]

```

The `{instance}` path segment names a klaus instance. It is resolved through the lifecycle manager; the routing table is bypassed on this path because the client already knows the instance it wants. OpenAI SDKs work by setting `baseURL = "http://klaus-gateway/v1/<instance>"`.

### `POST /v1/{instance}/chat/messages`

Thin wrapper around the MCP `messages` tool on the instance. Returns the stored conversation log as JSON.

```http
POST /v1/test-instance/chat/messages?thread_id=t1 HTTP/1.1
```

```json
{
  "messages": [
    {"role": "user", "content": "hi"},
    {"role": "assistant", "content": "hello"}
  ]
}
```

### Error mapping

| Situation                               | Status |
|----------------------------------------|--------|
| Unknown `{instance}` name              | 404    |
| Context cancelled by the client        | no body written |
| Upstream timeout                        | 504    |
| Other upstream error                    | 502    |

## Web channel adapter

Mounted at `/web/*`. This is the surface browser UIs and headless drivers call into. The full
contract, including the `prompt` event and HITL decisions and `GET /web/agents`, is in
[channels-web.md](channels-web.md).

### `POST /web/messages`

Body is a normalised `InboundMessage`:

```json
{
  "channelId": "web-session-42",
  "userId":    "alice",
  "threadId":  "thread-7",
  "text":      "hi",
  "subject":   "oauth-sub-optional",
  "attachments": []
}
```

Response is `text/event-stream` with typed deltas:

```
data: {"content":"hel"}

data: {"content":"lo"}

event: done
data: {}

```

The response also sets `X-Klaus-Instance: <name>` so the client knows which instance handled the turn.

### `GET /web/messages?channelId=...&userId=...&threadId=...`

Returns the stored history as JSON:

```json
{"messages": [{"role":"user","content":"hi", "sent_at":"..."}]}
```

### `GET /web/healthz`

200 once the adapter is started.

## Team-review endpoint

Through this surface a manager (giantswarm-repo-manager) posts an ask into a team's Slack channel
— the change spelled out, an *Approve* button, an *Open PR* link — or a notice that asks for
nothing. It is the inbound side of the [team review](slack-hitl-surface.md#10-team-review-a-managers-ask-to-a-team)
prompt. Mounted when `--reviews-enabled` is set (chart: `reviews.enabled`); requires the Slack
adapter and OBO account linking, since the Approve click runs as the linked person.

### Authentication: the caller's own ServiceAccount

The caller sends the projected Kubernetes ServiceAccount token it already has:

```http
Authorization: Bearer <projected ServiceAccount token, audience klaus-gateway>
```

The gateway verifies it through the `TokenReview` API for its audience (`--reviews-audience`,
default `klaus-gateway`) and admits only the ServiceAccounts listed in
`--reviews-allowed-callers` (`system:serviceaccount:<namespace>:<name>`). No personal token, no
shared secret: the API server is the verifier. The caller projects its token for the audience:

```yaml
volumes:
- name: klaus-gateway-token
  projected:
    sources:
    - serviceAccountToken:
        audience: klaus-gateway
        expirationSeconds: 600
        path: token
```

| Situation | Status |
|---|---|
| No bearer token, or one the API server does not vouch for | 401 (`WWW-Authenticate: Bearer`) |
| ServiceAccount not in the allow-list | 403 |
| API server unreachable for the review | 503 |
| Invalid body (see below) | 400 |
| Slack refused the post | 502 |

### `POST /reviews`

```json
{
  "team": "team-bumblebee",
  "channel": "C0123ABCDE",
  "text": "*Archive* `giantswarm/old-thing` (owned by team-bumblebee).",
  "link": "https://github.com/giantswarm/github/pull/4711",
  "approve": {
    "tool": "x_giantswarm-repo-manager_approve_change",
    "arguments": {"pr": 4711}
  }
}
```

- `team` — the GitHub team (slug) whose linked members may decide. Required.
- `channel` — the Slack channel **ID** (`C…`), not a name: the message is rewritten in place after
  the decision and `chat.update` needs the ID. Required.
- `text` — the change spelled out, Slack mrkdwn, at most 3000 characters. The caller is a trusted
  service, so the text is rendered as written. Required.
- `link` — an http(s) URL, rendered as the *Open PR* button for anything the Approve button does
  not cover. Optional.
- `approve.tool`, `approve.arguments` — the muster tool a member's click calls, verbatim, under
  that member's identity (their own token; muster and the tool see the person). `tool` required.
  The gateway reaches it through muster's `call_tool` meta-tool — the way every aggregated
  `x_<server>_<tool>` is exposed — and reads the tool's own `isError` verdict out of the envelope
  the meta-tool returns.

Unknown fields are refused. Response `201`:

```json
{"id": "8f3c2d…", "channel": "C0123ABCDE", "ts": "1726512345.000100"}
```

`id` is the gateway's handle on the review; a click on the message is resolved against it. Open
reviews are held in memory for seven days. A gateway restart drops them: a click on a dropped
review rewrites the message to say it expired, and the manager posts the ask again.

### `POST /notices`

Same authentication and fields except `approve`; renders without buttons, the link as a context
line. Response `201` with `channel` and `ts`.

## Admin surface

Served on the admin port (default `:8081`):

- `GET /healthz` -- liveness
- `GET /readyz`  -- readiness (probes the routing store)
- `GET /metrics` -- Prometheus scrape endpoint: the public mux's request counter and latency,
  `klaus_gateway_turn_total{channel,outcome}` and `klaus_gateway_turn_phase_seconds{channel,phase}`
  for the channel turns (see [deployment.md](deployment.md#observability))
