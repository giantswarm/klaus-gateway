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

Mounted at `/web/*`. This is the surface the lab webapp (and future UIs) call into.

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

## Admin surface

Served on the admin port (default `:8081`):

- `GET /healthz` -- liveness
- `GET /readyz`  -- readiness (probes the routing store)
- `GET /metrics` -- Prometheus scrape endpoint
